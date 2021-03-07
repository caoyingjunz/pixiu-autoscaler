/*
Copyright 2021.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package autoscaler

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"time"

	apps "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2beta2"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	appsinformers "k8s.io/client-go/informers/apps/v1"
	autoscalinginformers "k8s.io/client-go/informers/autoscaling/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	appslisters "k8s.io/client-go/listers/apps/v1"
	autoscalinglisters "k8s.io/client-go/listers/autoscaling/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/component-base/metrics/prometheus/ratelimiter"
	"k8s.io/klog"

	"github.com/caoyingjunz/kubez-autoscaler/pkg/controller"
)

const (
	maxRetries = 15
)

// AutoscalerController is responsible for synchronizing HPA objects stored
// in the system.
type AutoscalerController struct {
	client        clientset.Interface
	eventRecorder record.EventRecorder

	// To allow injection of syncKubez
	syncHandler func(hKey string) error
	enqueueHPA  func(hpa *autoscalingv2.HorizontalPodAutoscaler)

	// dLister can list/get deployments from the shared informer's store
	dLister appslisters.DeploymentLister
	// hpaLister is able to list/get HPAs from the shared informer's cache
	hpaLister autoscalinglisters.HorizontalPodAutoscalerLister

	// dListerSynced returns true if the Deployment store has been synced at least once.
	dListerSynced cache.InformerSynced
	// hpaListerSynced returns true if the HPA store has been synced at least once.
	hpaListerSynced cache.InformerSynced

	// AutoscalerController that need to be synced
	queue workqueue.RateLimitingInterface
}

// NewAutoscalerController creates a new AutoscalerController.
func NewAutoscalerController(dInformer appsinformers.DeploymentInformer, hpaInformer autoscalinginformers.HorizontalPodAutoscalerInformer, client clientset.Interface) (*AutoscalerController, error) {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: client.CoreV1().Events("")})

	if client != nil && client.CoreV1().RESTClient().GetRateLimiter() != nil {
		if err := ratelimiter.RegisterMetricAndTrackRateLimiterUsage("autoscaler_controller", client.CoreV1().RESTClient().GetRateLimiter()); err != nil {
			return nil, err
		}
	}

	ac := &AutoscalerController{
		client:        client,
		eventRecorder: eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "autoscaler-controller"}),
		queue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "autoscaler"),
	}

	// Deployment
	dInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ac.addDeployment,
		UpdateFunc: ac.updateDeployment,
		DeleteFunc: ac.deleteDeployment,
	})

	// HorizontalPodAutoscaler
	hpaInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ac.addHPA,
		UpdateFunc: ac.updateHPA,
		DeleteFunc: ac.deleteHPA,
	})

	ac.dLister = dInformer.Lister()
	ac.hpaLister = hpaInformer.Lister()

	// syncAutoscalers
	ac.syncHandler = ac.syncAutoscalers
	ac.enqueueHPA = ac.enqueue

	ac.dListerSynced = dInformer.Informer().HasSynced
	ac.hpaListerSynced = hpaInformer.Informer().HasSynced

	return ac, nil
}

// Run begins watching and syncing.
func (ac *AutoscalerController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer ac.queue.ShutDown()

	klog.Infof("Starting Autoscaler Controller")
	defer klog.Infof("Shutting down Autoscaler Controller")

	// TODO: tmp resolution, and will be removed
	sharedInformers := informers.NewSharedInformerFactory(ac.client, time.Minute)
	hpaInformer := sharedInformers.Autoscaling().V2beta2().HorizontalPodAutoscalers().Informer()
	hpaInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ac.addHPA,
		UpdateFunc: ac.updateHPA,
		DeleteFunc: ac.deleteHPA,
	})
	go hpaInformer.Run(stopCh)

	deployInformer := sharedInformers.Apps().V1().Deployments().Informer()
	deployInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ac.addDeployment,
		UpdateFunc: ac.updateDeployment,
		DeleteFunc: ac.deleteDeployment,
	})
	go deployInformer.Run(stopCh)

	for i := 0; i < workers; i++ {
		go wait.Until(ac.worker, time.Second, stopCh)
	}
	<-stopCh
}

// syncAutoscaler will sync the autoscaler with the given key.
func (ac *AutoscalerController) syncAutoscalers(key string) error {
	starTime := time.Now()
	klog.Infof("Start syncing autoscaler %q (%v)", key, starTime)
	defer func() {
		klog.Infof("Finished syncing autoscaler %q (%v)", key, time.Since(starTime))
	}()

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.Infof("namespace: %s, name: %s", namespace, name)
	return nil
}

func (ac *AutoscalerController) addHPA(obj interface{}) {}

func (ac *AutoscalerController) updateHPA(old, current interface{}) {
	cur := current.(*autoscalingv2.HorizontalPodAutoscaler)
	klog.V(0).Infof("Updating HPA %s/%s", cur.Namespace, cur.Name)

	ac.handerHPAUpdateEvent(cur)
	ac.enqueueHPA(cur)
}

func (ac *AutoscalerController) deleteHPA(obj interface{}) {
	h := obj.(*autoscalingv2.HorizontalPodAutoscaler)
	klog.V(0).Infof("Deleting HPA %s/%s", h.Namespace, h.Name)
	//ac.enqueueHPA(h)

	ac.handerHPADeleteEvent(h)
}

func (ac *AutoscalerController) addDeployment(obj interface{}) {
	d := obj.(*apps.Deployment)
	klog.V(0).Infof("Adding Deployment %s/%s", d.Namespace, d.Name)

	maxReplicas, ok := d.Annotations[controller.MaxReplicas]
	if !ok {
		// return directly
		return
	}

	maxReplicasInt, err := strconv.ParseInt(maxReplicas, 10, 32)
	if err != nil || maxReplicasInt == 0 {
		return
	}
	hpa := controller.CreateHorizontalPodAutoscaler(d.Name, d.Namespace, d.UID, "apps/v1", "Deployment", int32(maxReplicasInt))
	klog.Infof("Reconciling HPA %s/%s from %s", d.Namespace, d.Name, "Deployment")
	_, err = ac.client.AutoscalingV2beta2().HorizontalPodAutoscalers(d.Namespace).Create(context.TODO(), hpa, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			return
		}
		klog.Info(err)
	}
}

func (ac *AutoscalerController) updateDeployment(old, current interface{}) {
	oldD := old.(*apps.Deployment)
	curD := current.(*apps.Deployment)
	klog.V(0).Infof("Updating Deployment %s/%s", oldD.Namespace, curD.Name)

	oldAnnotations := oldD.Annotations
	curAnnotations := curD.Annotations
	if reflect.DeepEqual(oldAnnotations, curAnnotations) {
		return
	}

	oldMaxReplicas, oldOk := oldAnnotations[controller.MaxReplicas]
	curMaxReplicas, curOk := curAnnotations[controller.MaxReplicas]

	if oldOk && curOk {
		if oldMaxReplicas != curMaxReplicas {
			// UPDATE
			maxReplicasInt, err := strconv.ParseInt(curMaxReplicas, 10, 32)
			if err != nil || maxReplicasInt == 0 {
				klog.Errorf("maxReplicas is requred")
				return
			}
			hpa, err := ac.client.AutoscalingV2beta2().
				HorizontalPodAutoscalers(oldD.Namespace).
				Get(context.TODO(), oldD.Name, metav1.GetOptions{})
			if err != nil {
				// HPA 不存在
				curHpa := controller.CreateHorizontalPodAutoscaler(curD.Name, curD.Namespace, curD.UID, "apps/v1", "Deployment", int32(maxReplicasInt))
				_, err := ac.client.AutoscalingV2beta2().
					HorizontalPodAutoscalers(curD.Namespace).
					Create(context.TODO(), curHpa, metav1.CreateOptions{})
				klog.Error(err)
				return
			}
			hpa.Spec.MaxReplicas = int32(maxReplicasInt)
			_, err = ac.client.AutoscalingV2beta2().HorizontalPodAutoscalers(curD.Namespace).Update(context.TODO(), hpa, metav1.UpdateOptions{})
			if err != nil {
				klog.Error(err)
			}
		}
	} else if oldOk && !curOk {
		// DELETE HPA
		ac.client.AutoscalingV2beta2().HorizontalPodAutoscalers(oldD.Namespace).Delete(context.TODO(), oldD.Name, metav1.DeleteOptions{})
	} else if !oldOk && curOk {
		// CREATE HPA
		maxReplicasInt, err := strconv.ParseInt(curMaxReplicas, 10, 32)
		if err != nil || maxReplicasInt == 0 {
			klog.Errorf("maxReplicas is requred")
			return
		}
		hpa := controller.CreateHorizontalPodAutoscaler(curD.Name, curD.Namespace, curD.UID, "apps/v1", "Deployment", int32(maxReplicasInt))
		_, err = ac.client.AutoscalingV2beta2().
			HorizontalPodAutoscalers(curD.Namespace).
			Create(context.TODO(), hpa, metav1.CreateOptions{})
		if err != nil {
			klog.Error(err)
		}
	}
}

func (ac *AutoscalerController) deleteDeployment(obj interface{}) {
	d := obj.(*apps.Deployment)
	klog.V(0).Infof("Deleting Deployment %s/%s", d.Namespace, d.Name)

	_, ok := d.Annotations[controller.MaxReplicas]
	if ok {
		klog.Infof("The HPA %s/%s is reconciling(DELETE)", d.Namespace, d.Name)
		if err := ac.client.AutoscalingV2beta2().HorizontalPodAutoscalers(d.Namespace).Delete(context.TODO(), d.Name, metav1.DeleteOptions{}); err != nil {
			klog.Error(err)
		}
	}
}

// KubeAutoscaler is responsible for HPA objects stored.
type KubeAutoscaler struct {
	APIVersion  string
	Kind        string
	UID         types.UID
	Annotations map[string]string
}

// Parse KubeAutoscaler from the given kubernetes resources, the resources could be
// Deployment, ReplicaSet, StatefulSet, or ReplicationController.
func (ac *AutoscalerController) parseFromReference(hpa *autoscalingv2.HorizontalPodAutoscaler) (KubeAutoscaler, error) {
	kac := KubeAutoscaler{
		APIVersion: "apps/v1",
		Kind:       hpa.Spec.ScaleTargetRef.Kind,
	}

	switch hpa.Spec.ScaleTargetRef.Kind {
	case "Deployment":
		deployment, err := ac.client.AppsV1().Deployments(hpa.Namespace).Get(context.TODO(), hpa.Name, metav1.GetOptions{})
		if err != nil {
			return kac, err
		}

		kac.UID = deployment.UID
		kac.Annotations = deployment.Annotations
	}
	return kac, nil
}

func (ac *AutoscalerController) handerHPAUpdateEvent(cur *autoscalingv2.HorizontalPodAutoscaler) error {
	kac, err := ac.parseFromReference(cur)
	if err != nil {
		if errors.IsNotFound(err) {
			klog.Infof("HPA %s/%s has been deleted, reconciling (DELETE)", cur.Namespace, cur.Name)
			return ac.client.AutoscalingV2beta2().
				HorizontalPodAutoscalers(cur.Namespace).
				Delete(context.TODO(), cur.Name, metav1.DeleteOptions{})
		}
		return err
	}

	// TODO：后续整合类型转换
	maxReplicas, ok := kac.Annotations[controller.MaxReplicas]
	if !ok {
		// return directly
		return nil
	}

	maxReplicasInt, err := strconv.ParseInt(maxReplicas, 10, 32)
	if err != nil || maxReplicasInt == 0 {
		return fmt.Errorf("maxReplicas is requred")
	}
	// TODO: 不需更新，直接返回
	if int32(maxReplicasInt) == cur.Spec.MaxReplicas {
		return nil
	}

	cur.Spec.MaxReplicas = int32(maxReplicasInt)
	klog.Infof("HPA %s/%s has been updated, reconciling (UPDATE)", cur.Namespace, cur.Name)
	_, err = ac.client.AutoscalingV2beta2().
		HorizontalPodAutoscalers(cur.Namespace).
		Update(context.TODO(), cur, metav1.UpdateOptions{})
	return err
}

func (ac *AutoscalerController) handerHPADeleteEvent(hpa *autoscalingv2.HorizontalPodAutoscaler) error {
	kac, err := ac.parseFromReference(hpa)
	if err != nil {
		if errors.IsNotFound(err) {
			klog.Infof("HPA %s/%s has been deleted", hpa.Namespace, hpa.Name)
			return nil
		}
		return err
	}

	// TODO 可以封装，临时解决
	maxReplicas, ok := kac.Annotations[controller.MaxReplicas]
	if !ok {
		// return directly
		return nil
	}

	maxReplicasInt, err := strconv.ParseInt(maxReplicas, 10, 32)
	if err != nil || maxReplicasInt == 0 {
		return fmt.Errorf("maxReplicas is requred")
	}

	// Recover HPA from deployment
	nHpa := controller.CreateHorizontalPodAutoscaler(hpa.Name, hpa.Namespace, kac.UID, kac.APIVersion, kac.Kind, int32(maxReplicasInt))
	klog.Infof("Recovering HPA %s/%s from %s", hpa.Namespace, hpa.Name, kac.Kind)
	_, err = ac.client.AutoscalingV2beta2().HorizontalPodAutoscalers(hpa.Namespace).Create(context.TODO(), nHpa, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
		klog.Errorf("Recoverd HPA %s/%s from %s failed: %v", hpa.Namespace, hpa.Name, kac.Kind, err)
		return err
	}

	return nil
}

func (ac *AutoscalerController) enqueue(hpa *autoscalingv2.HorizontalPodAutoscaler) {
	key, err := controller.KeyFunc(hpa)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for object %#v: %v", hpa, err))
		return
	}

	ac.queue.Add(key)
}

// worker runs a worker thread that just dequeues items, processes then, and marks them done.
func (ac *AutoscalerController) worker() {
	for ac.processNextWorkItem() {
	}
}

func (ac *AutoscalerController) processNextWorkItem() bool {
	key, quit := ac.queue.Get()
	if quit {
		return false
	}
	defer ac.queue.Done(key)

	err := ac.syncHandler(key.(string))
	ac.handleErr(err, key)
	return true
}

func (ac *AutoscalerController) handleErr(err error, key interface{}) {
	if ac.queue.NumRequeues(key) < maxRetries {
		klog.V(0).Infof("Error syncing HPA %v: %v", key, err)
		ac.queue.AddRateLimited(key)
		return
	}

	utilruntime.HandleError(err)
	klog.V(0).Infof("Dropping HPA %q out of the queue: %v", key, err)
	ac.queue.Forget(key)
}

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
	"github.com/caoyingjunz/kubez-autoscaler/pkg/kubezstore"
)

const (
	maxRetries = 15

	AddEvent    string = "Add"
	UpdateEvent string = "Update"
	DeleteEvent string = "Delete"

	KubezEvent string = "kubezevent"
)

const (
	APIVersion              string = "apps/v1"
	Deployment              string = "Deployment"
	StatefulSet             string = "StatefulSet"
	HorizontalPodAutoscaler string = "HorizontalPodAutoscaler"
)

// AutoscalerController is responsible for synchronizing HPA objects stored
// in the system.
type AutoscalerController struct {
	client        clientset.Interface
	eventRecorder record.EventRecorder

	syncHandler       func(hpaKey string) error
	enqueueAutoscaler func(hpa *autoscalingv2.HorizontalPodAutoscaler)

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
	// Safe Store than to store the obj
	store kubezstore.SafeStoreInterface
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
		store:         kubezstore.NewSafeStore(),
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
	ac.enqueueAutoscaler = ac.enqueue

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
	klog.V(2).Infof("Start syncing autoscaler %q (%v)", key, starTime)
	defer func() {
		klog.V(2).Infof("Finished syncing autoscaler %q (%v)", key, time.Since(starTime))
	}()

	// Delete the obj from store even though the syncAutoscalers failed
	defer ac.store.Delete(key)

	obj, exists := ac.store.Get(key)
	if !exists {
		return nil
	}

	var err error
	hpa := obj.(*autoscalingv2.HorizontalPodAutoscaler)
	event := hpa.Annotations[KubezEvent]
	// TODO: remove kubezevent from event

	switch event {
	case AddEvent:
		klog.V(2).Infof("Adding HPA: %s/%s", hpa.Namespace, hpa.Name)
		_, err = ac.client.AutoscalingV2beta2().HorizontalPodAutoscalers(hpa.Namespace).Create(context.TODO(), hpa, metav1.CreateOptions{})
		if errors.IsAlreadyExists(err) {
			// The HPA has added
			return nil
		}
	case UpdateEvent:
		klog.V(2).Infof("Updating HPA: %s/%s", hpa.Namespace, hpa.Name)
		_, err = ac.client.AutoscalingV2beta2().HorizontalPodAutoscalers(hpa.Namespace).Update(context.TODO(), hpa, metav1.UpdateOptions{})
		if errors.IsNotFound(err) {
			// The HPA has deleted
			return nil
		}
	case DeleteEvent:
		klog.V(2).Infof("Deleting HPA: %s/%s", hpa.Namespace, hpa.Name)
		err = ac.client.AutoscalingV2beta2().HorizontalPodAutoscalers(hpa.Namespace).Delete(context.TODO(), hpa.Name, metav1.DeleteOptions{})
		if errors.IsNotFound(err) {
			// The HPA has deleted
			return nil
		}
	default:
		return fmt.Errorf("unknow HPA: %s/%s event", hpa.Namespace, hpa.Name)
	}

	return err
}

// To insert annotation to distinguish the event type
func (ac *AutoscalerController) InsertKubezAnnotation(hpa *autoscalingv2.HorizontalPodAutoscaler, event string) {
	if hpa.Annotations == nil {
		hpa.Annotations = map[string]string{
			KubezEvent: event,
		}
		return
	}
	hpa.Annotations[KubezEvent] = event
}

func (ac *AutoscalerController) addHPA(obj interface{}) {}

// updateHPA figures out what HPA(s) is updated and wake them up.
// old and cur must be *autoscalingv2.HorizontalPodAutoscaler types.
func (ac *AutoscalerController) updateHPA(old, cur interface{}) {
	oldH := old.(*autoscalingv2.HorizontalPodAutoscaler)
	curH := cur.(*autoscalingv2.HorizontalPodAutoscaler)
	if oldH.ResourceVersion == curH.ResourceVersion {
		// Periodic resync will send update events for all known HPAs.
		// Two different versions of the same HPA will always have different ResourceVersions.
		return
	}
	klog.V(0).Infof("Updating HPA %s", oldH.Name)

	key, err := controller.KeyFunc(curH)
	if err != nil {
		return
	}
	// To insert annotation to distinguish the event type is Update
	ac.InsertKubezAnnotation(curH, UpdateEvent)
	ac.store.Update(key, curH)

	ac.enqueueAutoscaler(curH)
}

func (ac *AutoscalerController) deleteHPA(obj interface{}) {
	h := obj.(*autoscalingv2.HorizontalPodAutoscaler)
	klog.V(0).Infof("Deleting HPA %s/%s", h.Namespace, h.Name)

	key, err := controller.KeyFunc(h)
	if err != nil {
		return
	}
	ac.InsertKubezAnnotation(h, DeleteEvent)
	ac.store.Update(key, h)

	ac.enqueueAutoscaler(h)
}

func (ac *AutoscalerController) addDeployment(obj interface{}) {
	d := obj.(*apps.Deployment)
	klog.V(0).Infof("Adding Deployment %s/%s", d.Namespace, d.Name)

	if hpa := ac.GetHorizontalPodAutoscalerForDeployment(d); hpa != nil {
		key, err := controller.KeyFunc(hpa)
		if err != nil {
			return
		}
		ac.InsertKubezAnnotation(hpa, AddEvent)
		ac.store.Update(key, hpa)

		ac.enqueueAutoscaler(hpa)
	}
}

func (ac *AutoscalerController) updateDeployment(old, new interface{}) {
	oldD := old.(*apps.Deployment)
	newD := new.(*apps.Deployment)
	klog.V(0).Infof("Updating Deployment %s/%s", oldD.Namespace, oldD.Name)

	oldHPA := ac.GetHorizontalPodAutoscalerForDeployment(oldD)
	newHPA := ac.GetHorizontalPodAutoscalerForDeployment(newD)

	// Do noting, return directly
	if oldHPA == nil && newHPA == nil {
		return
	}

	// Add
	if oldHPA == nil && newHPA != nil {
		key, err := controller.KeyFunc(newHPA)
		if err != nil {
			return
		}
		ac.InsertKubezAnnotation(newHPA, AddEvent)
		ac.store.Add(key, newHPA)

		ac.enqueueAutoscaler(newHPA)
		return
	}

	// Delete
	if oldHPA != nil && newHPA == nil {
		key, err := controller.KeyFunc(oldHPA)
		if err != nil {
			return
		}
		ac.InsertKubezAnnotation(oldHPA, DeleteEvent)
		ac.store.Add(key, oldHPA)

		ac.enqueueAutoscaler(oldHPA)
		return
	}

	// Update
	if oldHPA != nil && newHPA != nil {
		if reflect.DeepEqual(oldHPA.Spec, newHPA.Spec) {
			// No need to updated
			return
		}

		key, err := controller.KeyFunc(newHPA)
		if err != nil {
			return
		}
		ac.InsertKubezAnnotation(newHPA, UpdateEvent)
		ac.store.Add(key, newHPA)

		ac.enqueueAutoscaler(newHPA)
	}
}

func (ac *AutoscalerController) deleteDeployment(obj interface{}) {
	d := obj.(*apps.Deployment)
	klog.V(0).Infof("Deleting Deployment %s/%s", d.Namespace, d.Name)

	if hpa := ac.GetHorizontalPodAutoscalerForDeployment(d); hpa != nil {
		key, err := controller.KeyFunc(hpa)
		if err != nil {
			return
		}
		ac.InsertKubezAnnotation(hpa, DeleteEvent)
		ac.store.Update(key, hpa)

		ac.enqueueAutoscaler(hpa)
	}
}

func (ac *AutoscalerController) GetHorizontalPodAutoscalerForDeployment(d *apps.Deployment) *autoscalingv2.HorizontalPodAutoscaler {
	if d == nil || d.Annotations == nil {
		return nil
	}

	// TODO: 暂时只判断是否含有 MaxReplicas
	maxReplicas, ok := d.Annotations[controller.MaxReplicas]
	if !ok {
		return nil
	}

	// TODO: ignore the error for now
	maxReplicasInt, err := strconv.ParseInt(maxReplicas, 10, 32)
	if err != nil || maxReplicasInt == 0 {
		return nil
	}

	hpa := controller.CreateHorizontalPodAutoscaler(d.Name, d.Namespace, d.UID, APIVersion, Deployment, int32(maxReplicasInt))

	return hpa
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

func (ac *AutoscalerController) enqueue(hpa *autoscalingv2.HorizontalPodAutoscaler) {
	key, err := controller.KeyFunc(hpa)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", hpa, err))
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
	if err == nil {
		ac.queue.Forget(key)
		return
	}

	if ac.queue.NumRequeues(key) < maxRetries {
		klog.V(0).Infof("Error syncing HPA %v: %v", key, err)
		ac.queue.AddRateLimited(key)
		return
	}

	utilruntime.HandleError(err)
	klog.V(0).Infof("Dropping HPA %q out of the queue: %v", key, err)
	ac.queue.Forget(key)
}

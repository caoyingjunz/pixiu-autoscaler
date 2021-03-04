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
	"strconv"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2beta2"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	autoscalinginformers "k8s.io/client-go/informers/autoscaling/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	autoscalinglisters "k8s.io/client-go/listers/autoscaling/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/component-base/metrics/prometheus/ratelimiter"
	"k8s.io/klog"

	"github.com/caoyingjunz/kubez-autoscaler/pkg/controller"
)

// AutoscalerController is responsible for synchronizing HPA objects stored
// in the system.
type AutoscalerController struct {
	client        clientset.Interface
	eventRecorder record.EventRecorder

	// To allow injection of syncKubez
	syncHandler func(dKey string) error

	enqueueHPA func(hpa *autoscalingv2.HorizontalPodAutoscaler)
	// hpaLister is able to list/get HPAs from the shared cache from the informer passed in to
	// NewHorizontalController.
	hpaLister       autoscalinglisters.HorizontalPodAutoscalerLister
	hpaListerSynced cache.InformerSynced

	// KubezController that need to be synced
	queue workqueue.RateLimitingInterface
}

// NewAutoscalerController creates a new AutoscalerController.
func NewAutoscalerController(hpaInformer autoscalinginformers.HorizontalPodAutoscalerInformer, client clientset.Interface) (*AutoscalerController, error) {
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

	hpaInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ac.addHPA,
		UpdateFunc: ac.updateHPA,
		DeleteFunc: ac.deleteHPA,
	})

	ac.syncHandler = ac.syncAutoscaler
	ac.enqueueHPA = ac.enqueue

	ac.hpaLister = hpaInformer.Lister()
	ac.hpaListerSynced = hpaInformer.Informer().HasSynced

	return ac, nil
}

// Run begins watching and syncing.
func (ac *AutoscalerController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer ac.queue.ShutDown()

	klog.Infof("Starting Autoscaler Controller")
	defer klog.Infof("Shutting down Autoscaler Controller")

	for i := 0; i < workers; i++ {
		go wait.Until(ac.worker, time.Second, stopCh)
	}

	// TODO: test for tmp will removed later
	sharedInformers := informers.NewSharedInformerFactory(ac.client, time.Minute)
	informer := sharedInformers.Autoscaling().V2beta2().HorizontalPodAutoscalers().Informer()
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ac.addHPA,
		UpdateFunc: ac.updateHPA,
		DeleteFunc: ac.deleteHPA,
	})
	go informer.Run(stopCh)

	<-stopCh
}

// syncAutoscaler will sync the autoscaler with the given key.
func (ac *AutoscalerController) syncAutoscaler(key string) error {
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

func (ac *AutoscalerController) addHPA(obj interface{}) {
	h := obj.(*autoscalingv2.HorizontalPodAutoscaler)
	klog.V(0).Infof("Adding HPA %s", h.Name)
}

func (ac *AutoscalerController) updateHPA(old, current interface{}) {
	oldH := old.(*autoscalingv2.HorizontalPodAutoscaler)
	newH := current.(*autoscalingv2.HorizontalPodAutoscaler)
	klog.V(0).Infof("Updating old HPA %s and new HPA %s", oldH.Name, newH.Name)
}

func (ac *AutoscalerController) deleteHPA(obj interface{}) {
	h := obj.(*autoscalingv2.HorizontalPodAutoscaler)
	klog.V(0).Infof("Deleting HPA %s", h.Name)
	//ac.enqueueHPA(h)

	ac.handerHPAEvent(h)
}

func (ac *AutoscalerController) handerHPAEvent(hpa *autoscalingv2.HorizontalPodAutoscaler) error {
	var apiVersion string
	var uid types.UID
	var hpaAnnotations map[string]string

	kind := hpa.Spec.ScaleTargetRef.Kind
	switch kind {
	case "Deployment":
		deployment, err := ac.client.AppsV1().Deployments(hpa.Namespace).Get(context.TODO(), hpa.Name, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				klog.Infof("Deployment %s/%s has been deleted", hpa.Namespace, hpa.Name)
				return nil
			}
			return err
		}
		// TODO
		apiVersion = "apps/v1"
		uid = deployment.UID
		hpaAnnotations = deployment.Annotations
	}

	// TODO 可以封装，临时解决
	maxReplicas, ok := hpaAnnotations[controller.MaxReplicas]
	if !ok {
		// return directly
		return nil
	}

	maxReplicasInt, err := strconv.ParseInt(maxReplicas, 10, 32)
	if err != nil || maxReplicasInt == 0 {
		return fmt.Errorf("maxReplicas is requred")
	}

	// Recover HPA from deployment
	nHpa := controller.CreateHorizontalPodAutoscaler(hpa.Name, hpa.Namespace, uid, apiVersion, kind, int32(maxReplicasInt))
	klog.Infof("Recovering HPA %s/%s from %s", hpa.Namespace, hpa.Name, kind)
	_, err = ac.client.AutoscalingV2beta2().HorizontalPodAutoscalers(hpa.Namespace).Create(context.TODO(), nHpa, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
		klog.Errorf("Recoverd HPA %s/%s from %s failed: %v", hpa.Namespace, hpa.Name, kind, err)
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

func (ac *AutoscalerController) worker() {
	for ac.processNextWorkItem() {
	}
}

func (ac *AutoscalerController) processNextWorkItem() bool {
	key, quit := ac.queue.Get()
	if quit {
		fmt.Println("test")
		return false
	}
	defer ac.queue.Done(key)
	fmt.Println(key)

	return true
}

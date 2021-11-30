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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2beta2"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	appsinformers "k8s.io/client-go/informers/apps/v1"
	autoscalinginformers "k8s.io/client-go/informers/autoscaling/v2beta2"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	appslisters "k8s.io/client-go/listers/apps/v1"
	autoscalinglisters "k8s.io/client-go/listers/autoscaling/v2beta2"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/component-base/metrics/prometheus/ratelimiter"
	"k8s.io/klog/v2"

	"github.com/caoyingjunz/pixiu-autoscaler/pkg/controller"
)

const (
	maxRetries = 15
)

const (
	appsAPIVersion string = "apps/v1"

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
	// sLister can list/get statefulset from the shared informer's store
	sLister appslisters.StatefulSetLister
	// hpaLister is able to list/get HPAs from the shared informer's cache
	hpaLister autoscalinglisters.HorizontalPodAutoscalerLister

	// dListerSynced returns true if the Deployment store has been synced at least once.
	dListerSynced cache.InformerSynced
	// sListerSynced returns true if the StatefulSet store has been synced at least once.
	sListerSynced cache.InformerSynced
	// hpaListerSynced returns true if the HPA store has been synced at least once.
	hpaListerSynced cache.InformerSynced

	addHpaQueue    workqueue.RateLimitingInterface
	updateHpaQueue workqueue.RateLimitingInterface
	deleteHpaQueue workqueue.RateLimitingInterface
}

// NewAutoscalerController creates a new AutoscalerController.
func NewAutoscalerController(
	dInformer appsinformers.DeploymentInformer,
	sInformer appsinformers.StatefulSetInformer,
	hpaInformer autoscalinginformers.HorizontalPodAutoscalerInformer,
	client clientset.Interface) (*AutoscalerController, error) {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: client.CoreV1().Events("")})

	if client != nil && client.CoreV1().RESTClient().GetRateLimiter() != nil {
		if err := ratelimiter.RegisterMetricAndTrackRateLimiterUsage("pixiu_autoscaler", client.CoreV1().RESTClient().GetRateLimiter()); err != nil {
			return nil, err
		}
	}

	ac := &AutoscalerController{
		client:         client,
		eventRecorder:  eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "pixiu-autoscaler"}),
		addHpaQueue:    workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "addHPA"),
		updateHpaQueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "updateHPA"),
		deleteHpaQueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "deleteHPA"),
	}

	// Deployment
	dInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ac.addDeployment,
		UpdateFunc: ac.updateDeployment,
		DeleteFunc: ac.deleteDeployment,
	})

	// StatefulSet
	sInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ac.addStatefulset,
		UpdateFunc: ac.updateStatefulset,
		DeleteFunc: ac.deleteStatefulset,
	})

	// HorizontalPodAutoscaler
	hpaInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ac.addHPA,
		UpdateFunc: ac.updateHPA,
		DeleteFunc: ac.deleteHPA,
	})

	ac.dLister = dInformer.Lister()
	ac.sLister = sInformer.Lister()
	ac.hpaLister = hpaInformer.Lister()

	ac.dListerSynced = dInformer.Informer().HasSynced
	ac.sListerSynced = sInformer.Informer().HasSynced
	ac.hpaListerSynced = hpaInformer.Informer().HasSynced

	return ac, nil
}

// Run begins watching and syncing.
func (ac *AutoscalerController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer ac.addHpaQueue.ShutDown()
	defer ac.updateHpaQueue.ShutDown()
	defer ac.deleteHpaQueue.ShutDown()

	klog.Infof("Starting Pixiu Autoscaler Controller")
	defer klog.Infof("Shutting down Pixiu Autoscaler Controller")

	// Wait for all involved caches to be synced, before processing items from the queue is started
	if !cache.WaitForNamedCacheSync("pixiu-autoscaler-manager", stopCh, ac.dListerSynced, ac.sListerSynced, ac.hpaListerSynced) {
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(ac.addHPAWorker, time.Second, stopCh)
	}

	<-stopCh
}

// syncAutoscaler will sync the autoscaler with the given key.
// This function is not meant to be invoked concurrently with the same key.
func (ac *AutoscalerController) syncAutoscalers(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.ErrorS(err, "Failed to split meta namespace cache key", "cacheKey", key)
		return err
	}

	startTime := time.Now()
	klog.V(4).InfoS("Started syncing pixiu autoscaler", "pixiuautoscaler", klog.KRef(namespace, name), "startTime", startTime)
	defer func() {
		klog.V(4).InfoS("Finished syncing pixiu autoscaler", "pixiuautoscaler", klog.KRef(namespace, name), "duration", time.Since(startTime))
	}()

	return err
}

// GetNewestHPA will get newest HPA from kubernetes resources
func (ac *AutoscalerController) GetNewestHPAFromResource(
	hpa *autoscalingv2.HorizontalPodAutoscaler) (*autoscalingv2.HorizontalPodAutoscaler, error) {
	var annotations map[string]string
	var uid types.UID
	kind := hpa.Spec.ScaleTargetRef.Kind
	switch kind {
	case Deployment:
		d, err := ac.dLister.Deployments(hpa.Namespace).Get(hpa.Name)
		if err != nil {
			return nil, err
		}
		// check
		if !controller.IsOwnerReference(d.UID, hpa.OwnerReferences) {
			return nil, nil
		}
		uid = d.UID
		annotations = d.Annotations
	case StatefulSet:
		s, err := ac.sLister.StatefulSets(hpa.Namespace).Get(hpa.Name)
		if err != nil {
			return nil, err
		}
		if !controller.IsOwnerReference(s.UID, hpa.OwnerReferences) {
			return nil, nil
		}
		uid = s.UID
		annotations = s.Annotations
	}
	if !controller.IsNeedForHPAs(annotations) {
		return nil, nil
	}

	return controller.CreateHorizontalPodAutoscaler(
		hpa.Name, hpa.Namespace, uid, appsAPIVersion, kind, annotations)
}

func (ac *AutoscalerController) HandlerAddEvents(obj interface{}) {
	ascCtx := controller.NewAutoscalerContext(obj)
	klog.V(2).Infof("Adding %s %s/%s", ascCtx.Kind, ascCtx.Namespace, ascCtx.Name)
	if !controller.IsNeedForHPAs(ascCtx.Annotations) {
		return
	}

	hpa, err := controller.CreateHorizontalPodAutoscaler(
		ascCtx.Name, ascCtx.Namespace, ascCtx.UID, appsAPIVersion, ascCtx.Kind, ascCtx.Annotations)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	if hpa == nil {
		return
	}

	key, err := controller.KeyFunc(hpa)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", hpa, err))
		return
	}

	ac.enqueueAutoscaler(hpa)
}

func (ac *AutoscalerController) HandlerUpdateEvents(old, cur interface{}) {
	oldCtx := controller.NewAutoscalerContext(old)
	curCtx := controller.NewAutoscalerContext(cur)
	klog.V(2).Infof("Updating %s %s/%s", oldCtx.Kind, oldCtx.Namespace, oldCtx.Name)

	if reflect.DeepEqual(oldCtx.Annotations, curCtx.Annotations) {
		return
	}
	oldExists := controller.IsNeedForHPAs(oldCtx.Annotations)
	curExists := controller.IsNeedForHPAs(curCtx.Annotations)

	// Delete HPAs
	if oldExists && !curExists {
		hpa, err := ac.hpaLister.HorizontalPodAutoscalers(oldCtx.Namespace).Get(oldCtx.Name)
		if err != nil {
			if errors.IsNotFound(err) {
				return
			}
			utilruntime.HandleError(err)
			return
		}

		key, err := controller.KeyFunc(hpa)
		if err != nil {
			utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", hpa, err))
			return
		}
		ac.wrapInnerEvent(hpa, DeleteEvent)
		ac.store.Add(key, hpa)

		ac.enqueueAutoscaler(hpa)
		return
	}

	// Add or Update HPAs
	hpa, err := controller.CreateHorizontalPodAutoscaler(
		curCtx.Name, curCtx.Namespace, curCtx.UID, appsAPIVersion, curCtx.Kind, curCtx.Annotations)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}

	key, err := controller.KeyFunc(hpa)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", hpa, err))
		return
	}

	if !oldExists && curExists {
		ac.wrapInnerEvent(hpa, AddEvent)
	} else if oldExists && curExists {
		ac.wrapInnerEvent(hpa, UpdateEvent)
	}
	ac.store.Add(key, hpa)

	ac.enqueueAutoscaler(hpa)
}

func (ac *AutoscalerController) HandlerDeleteEvents(obj interface{}) {
	ascCtx := controller.NewAutoscalerContext(obj)
	klog.V(2).Infof("Deleting %s %s/%s", ascCtx.Kind, ascCtx.Namespace, ascCtx.Name)

	hpa, err := ac.hpaLister.HorizontalPodAutoscalers(ascCtx.Namespace).Get(ascCtx.Name)
	if err != nil {
		if errors.IsNotFound(err) {
			// HPA has been deleted
			return
		}
		utilruntime.HandleError(err)
		return
	}
	if !controller.IsOwnerReference(ascCtx.UID, hpa.OwnerReferences) {
		return
	}

	key, err := controller.KeyFunc(hpa)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", hpa, err))
		return
	}
	ac.wrapInnerEvent(hpa, DeleteEvent)

	ac.enqueueAutoscaler(hpa)
}

func (ac *AutoscalerController) addHPA(obj interface{}) {
	h := obj.(*autoscalingv2.HorizontalPodAutoscaler)
	if !controller.ManagerByKubezController(h) {
		return
	}

	key, err := controller.KeyFunc(h)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", h, err))
		return
	}

	klog.V(0).Infof("Adding HPA(manager by pixiu) %s/%s", h.Namespace, h.Name)
	ac.addHpaQueue.Add(key)
}

// updateHPA figures out what HPA(s) is updated and wake them up.
// old and cur must be *autoscalingv2.HorizontalPodAutoscaler types.
func (ac *AutoscalerController) updateHPA(old, cur interface{}) {
	oldHPA := old.(*autoscalingv2.HorizontalPodAutoscaler)
	curHPA := cur.(*autoscalingv2.HorizontalPodAutoscaler)
	if oldHPA.ResourceVersion == curHPA.ResourceVersion {
		// Periodic resync will send update events for all known HPAs.
		// Two different versions of the same HPA will always have different ResourceVersions.
		return
	}
	if !controller.ManagerByKubezController(oldHPA) {
		return
	}
	klog.V(0).Infof("Updating HPA %s/%s", oldHPA.Namespace, oldHPA.Name)

	key, err := controller.KeyFunc(curHPA)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", curHPA, err))
		return
	}
	ac.updateHpaQueue.Add(key)
}

func (ac *AutoscalerController) deleteHPA(obj interface{}) {
	h := obj.(*autoscalingv2.HorizontalPodAutoscaler)
	// TODO: rename the manager func
	if !controller.ManagerByKubezController(h) {
		return
	}

	klog.V(0).Infof("Deleting HPA %s/%s", h.Namespace, h.Name)
	ac.deleteHpaQueue.Add(h)
}

// worker runs a worker thread that just dequeues items, processes then, and marks them done.
func (ac *AutoscalerController) addHPAWorker() {
	for ac.processNextAddHPAWorkItem() {
	}
}

func (ac *AutoscalerController) processNextAddHPAWorkItem() bool {
	key, quit := ac.addHpaQueue.Get()
	if quit {
		return false
	}
	defer ac.addHpaQueue.Done(key)

	err := ac.syncHandler(key.(string))
	ac.handleAddHPAErr(err, key)
	return true
}

func (ac *AutoscalerController) handleAddHPAErr(err error, key interface{}) {
	if err == nil {
		ac.addHpaQueue.Forget(key)
		return
	}

	if ac.addHpaQueue.NumRequeues(key) < maxRetries {
		klog.V(0).Infof("Error syncing HPA %v: %v", key, err)
		ac.addHpaQueue.AddRateLimited(key)
		return
	}

	utilruntime.HandleError(err)
	klog.V(0).Infof("Dropping HPA %q out of the queue: %v", key, err)
	ac.addHpaQueue.Forget(key)
}

// extractHPAForDeployment returns the deployment managed by the given deployment.
func (ac *AutoscalerController) extractHPAForDeployment(d *appsv1.Deployment) *autoscalingv2.HorizontalPodAutoscaler {
	return nil
}

// This functions just wrap Handler Deployment Events for improve the readability of codes
func (ac *AutoscalerController) addDeployment(obj interface{}) {
	d := obj.(*appsv1.Deployment)
	klog.V(4).InfoS("Adding deployment", "deployment", klog.KObj(d))

	if !controller.IsNeedForHPAs(d.Annotations) {
		return
	}
	h := ac.extractHPAForDeployment(d)

	key, err := controller.KeyFunc(h)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", h, err))
		return
	}

	klog.V(0).Infof("Adding HPA(manager by pixiu) %s/%s", h.Namespace, h.Name)
	ac.addHpaQueue.Add(key)
}

func (ac *AutoscalerController) updateDeployment(old, cur interface{}) {
	klog.V(2).Infof("Handering update Deployment event")
	// Periodic resync will send update events for all known Deployments.
	// Two different versions of the same Deployment will always have different RVs.
	oldD := old.(*appsv1.Deployment)
	curD := cur.(*appsv1.Deployment)

	if oldD.ResourceVersion == curD.ResourceVersion {
		return
	}
	if !controller.IsNeedForHPAs(curD.Annotations) {
		return
	}
	h := ac.extractHPAForDeployment(curD)

	key, err := controller.KeyFunc(h)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", h, err))
		return
	}

	klog.V(0).Infof("Adding HPA(manager by pixiu) %s/%s", h.Namespace, h.Name)
	ac.updateHpaQueue.Add(key)
}

func (ac *AutoscalerController) deleteDeployment(obj interface{}) {
	klog.V(2).Infof("Handering delete Deployment event")
	d := obj.(*appsv1.Deployment)
	if !controller.IsNeedForHPAs(d.Annotations) {
		return
	}
	hpa := ac.extractHPAForDeployment(d)

	klog.V(0).Infof("Deletinig HPA(manager by pixiu) %s/%s", hpa.Namespace, hpa.Name)
	ac.deleteHpaQueue.Add(hpa)
}

// This functions just wrap Handler StatefulSet Events for improve the readability of codes
func (ac *AutoscalerController) addStatefulset(obj interface{}) {
	klog.V(2).Infof("Handering add StatefulSet event")
	ac.HandlerAddEvents(obj)
}

func (ac *AutoscalerController) updateStatefulset(old, cur interface{}) {
	klog.V(2).Infof("Handering update StatefulSet event")
	// Periodic resync will send update events for all known Deployments.
	// Two different versions of the same Deployment will always have different RVs.
	if old.(*appsv1.StatefulSet).ResourceVersion == cur.(*appsv1.StatefulSet).ResourceVersion {
		return
	}
	ac.HandlerUpdateEvents(old, cur)
}

func (ac *AutoscalerController) deleteStatefulset(obj interface{}) {
	klog.V(2).Infof("Handering delete StatefulSet event")
	ac.HandlerDeleteEvents(obj)
}

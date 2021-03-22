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

	autoscalingv2 "k8s.io/api/autoscaling/v2beta2"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	"k8s.io/klog"

	"github.com/caoyingjunz/kubez-autoscaler/pkg/controller"
	"github.com/caoyingjunz/kubez-autoscaler/pkg/kubezstore"
)

const (
	maxRetries = 15

	AddEvent           string = "Add"
	UpdateEvent        string = "Update"
	DeleteEvent        string = "Delete"
	RecoverDeleteEvent string = "RecoverDelete"
	RecoverUpdateEvent string = "RecoverUpdate"

	kubezEvent string = "kubezEvent"
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

	// AutoscalerController that need to be synced
	queue workqueue.RateLimitingInterface
	// Safe Store than to store the obj
	store kubezstore.SafeStoreInterface
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

	//Statefulset
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

	// syncAutoscalers
	ac.syncHandler = ac.syncAutoscalers
	ac.enqueueAutoscaler = ac.enqueue

	ac.dListerSynced = dInformer.Informer().HasSynced
	ac.sListerSynced = sInformer.Informer().HasSynced
	ac.hpaListerSynced = hpaInformer.Informer().HasSynced

	return ac, nil
}

// Run begins watching and syncing.
func (ac *AutoscalerController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer ac.queue.ShutDown()

	klog.Infof("Starting Autoscaler Controller")
	defer klog.Infof("Shutting down Autoscaler Controller")

	// Wait for all involved caches to be synced, before processing items from the queue is started
	if !cache.WaitForNamedCacheSync("kubez-autoscaler-manager", stopCh, ac.dListerSynced, ac.sListerSynced, ac.hpaListerSynced) {
		return
	}

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

	hpa, exists := ac.store.Get(key)
	if !exists {
		// Do nothing and return directly
		return nil
	}

	var err error
	event := ac.PopKubezAnnotation(hpa)
	klog.V(0).Infof("Handlering HPA: %s/%s, event: %s", hpa.Namespace, hpa.Name, event)

	switch event {
	case AddEvent:
		_, err = ac.client.AutoscalingV2beta2().HorizontalPodAutoscalers(hpa.Namespace).Create(context.TODO(), hpa, metav1.CreateOptions{})
		if errors.IsAlreadyExists(err) {
			// The HPA has added
			return nil
		}
	case UpdateEvent:
		_, err = ac.client.AutoscalingV2beta2().HorizontalPodAutoscalers(hpa.Namespace).Update(context.TODO(), hpa, metav1.UpdateOptions{})
		if errors.IsNotFound(err) {
			// The HPA has deleted
			return nil
		}
	case DeleteEvent:
		err = ac.client.AutoscalingV2beta2().HorizontalPodAutoscalers(hpa.Namespace).Delete(context.TODO(), hpa.Name, metav1.DeleteOptions{})
		if errors.IsNotFound(err) {
			// The HPA has deleted
			return nil
		}
	case RecoverUpdateEvent:
		// Since the HPA has been updated, we need to get origin spec to check whether it shouled be recover
		newHPA, err := ac.GetNewestHPAFromResource(hpa)
		if err != nil {
			return err
		}
		if newHPA == nil {
			return nil
		}
		if reflect.DeepEqual(hpa.Spec, newHPA.Spec) {
			klog.V(2).Infof("HPA: %s/%s spec is not changed, no need to updated", hpa.Namespace, hpa.Name)
			return nil
		}

		_, err = ac.client.AutoscalingV2beta2().HorizontalPodAutoscalers(newHPA.Namespace).Update(context.TODO(), newHPA, metav1.UpdateOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				return nil
			}
			return err
		}
	case RecoverDeleteEvent:
		newHPA, err := ac.GetNewestHPAFromResource(hpa)
		if err != nil {
			return err
		}
		if newHPA == nil {
			return nil
		}

		_, err = ac.client.AutoscalingV2beta2().HorizontalPodAutoscalers(newHPA.Namespace).Create(context.TODO(), newHPA, metav1.CreateOptions{})
		if err != nil {
			if errors.IsAlreadyExists(err) {
				return nil
			}
			return err
		}
	default:
		return fmt.Errorf("unsupported handlers event %s", event)
	}

	return err
}

// GetNewestHPA will get newest HPA from kubernetes resources
func (ac *AutoscalerController) GetNewestHPAFromResource(hpa *autoscalingv2.HorizontalPodAutoscaler) (*autoscalingv2.HorizontalPodAutoscaler, error) {
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

	hpaAnnotations, err := controller.PreAndExtractAnnotations(annotations)
	if err != nil {
		return nil, err
	}

	return controller.CreateHorizontalPodAutoscaler(hpa.Name, hpa.Namespace, uid, appsAPIVersion, kind, hpaAnnotations), nil
}

// To insert annotation to distinguish the event type
func (ac *AutoscalerController) InsertKubezAnnotation(hpa *autoscalingv2.HorizontalPodAutoscaler, event string) {
	if hpa.Annotations == nil {
		hpa.Annotations = map[string]string{
			kubezEvent: event,
		}
		return
	}
	hpa.Annotations[kubezEvent] = event
}

// To pop kubez annotation and clean up kubez marker from HPA
func (ac *AutoscalerController) PopKubezAnnotation(hpa *autoscalingv2.HorizontalPodAutoscaler) string {
	event, exists := hpa.Annotations[kubezEvent]
	// This shouldn't happen, because we only insert annotation for hpa
	if exists {
		delete(hpa.Annotations, kubezEvent)
	}
	return event
}

func (ac *AutoscalerController) HandlerAddEvents(obj interface{}) {
	ascCtx := controller.NewAutoscalerContext(obj)
	klog.V(2).Infof("Adding %s %s/%s", ascCtx.Kind, ascCtx.Namespace, ascCtx.Name)
	if !controller.IsNeedForHPAs(ascCtx.Annotations) {
		return
	}

	hpaAnnotations, err := controller.PreAndExtractAnnotations(ascCtx.Annotations)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	if len(hpaAnnotations) == 0 {
		return
	}

	if hpa := controller.CreateHorizontalPodAutoscaler(ascCtx.Name, ascCtx.Namespace, ascCtx.UID, appsAPIVersion, ascCtx.Kind, hpaAnnotations); hpa != nil {
		key, err := controller.KeyFunc(hpa)
		if err != nil {
			utilruntime.HandleError(err)
			return
		}
		ac.InsertKubezAnnotation(hpa, AddEvent)
		ac.store.Update(key, hpa)

		ac.enqueueAutoscaler(hpa)
	}
}

func (ac *AutoscalerController) HandlerUpdateEvents(old, cur interface{}) {
	oldCtx := controller.NewAutoscalerContext(old)
	curCtx := controller.NewAutoscalerContext(cur)
	klog.V(2).Infof("Updating %s %s/%s", oldCtx.Kind, oldCtx.Namespace, oldCtx.Name)

	if reflect.DeepEqual(oldCtx.Annotations, curCtx.Annotations) {
		return
	}

	// No need to handler error for old, it always success or nil
	oldAnnotations, _ := controller.PreAndExtractAnnotations(oldCtx.Annotations)
	curAnnotations, err := controller.PreAndExtractAnnotations(curCtx.Annotations)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}

	// Do noting, return directly if HPA Annotations stuff not changed
	if reflect.DeepEqual(oldAnnotations, curAnnotations) {
		return
	}

	oldExists := len(oldAnnotations) != 0
	curExists := len(curAnnotations) != 0

	// Delete HPAs
	if oldExists && !curExists {
		hpa := controller.CreateHorizontalPodAutoscaler(oldCtx.Name, oldCtx.Namespace, oldCtx.UID, appsAPIVersion, oldCtx.Kind, oldAnnotations)
		key, err := controller.KeyFunc(hpa)
		if err != nil {
			return
		}
		ac.InsertKubezAnnotation(hpa, DeleteEvent)
		ac.store.Add(key, hpa)

		ac.enqueueAutoscaler(hpa)
		return
	}

	// Add or Update HPAs
	hpa := controller.CreateHorizontalPodAutoscaler(curCtx.Name, curCtx.Namespace, curCtx.UID, appsAPIVersion, curCtx.Kind, curAnnotations)
	key, err := controller.KeyFunc(hpa)
	if err != nil {
		return
	}

	if !oldExists && curExists {
		ac.InsertKubezAnnotation(hpa, AddEvent)
	} else if oldExists && curExists {
		ac.InsertKubezAnnotation(hpa, UpdateEvent)
	}
	ac.store.Add(key, hpa)

	ac.enqueueAutoscaler(hpa)
}

func (ac *AutoscalerController) HandlerDeleteEvents(obj interface{}) {
	ascCtx := controller.NewAutoscalerContext(obj)
	klog.V(2).Infof("Deleting %s %s/%s", ascCtx.Kind, ascCtx.Namespace, ascCtx.Name)

	// TODO 直接删除
	hpaAnnotations, err := controller.PreAndExtractAnnotations(ascCtx.Annotations)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	if len(hpaAnnotations) == 0 {
		return
	}

	if hpa := controller.CreateHorizontalPodAutoscaler(ascCtx.Name, ascCtx.Namespace, ascCtx.UID, appsAPIVersion, ascCtx.Kind, hpaAnnotations); hpa != nil {
		key, err := controller.KeyFunc(hpa)
		if err != nil {
			utilruntime.HandleError(err)
			return
		}
		ac.InsertKubezAnnotation(hpa, DeleteEvent)
		ac.store.Update(key, hpa)

		ac.enqueueAutoscaler(hpa)
	}
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
	ac.InsertKubezAnnotation(curH, RecoverUpdateEvent)
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
	ac.InsertKubezAnnotation(h, RecoverDeleteEvent)
	ac.store.Update(key, h)

	ac.enqueueAutoscaler(h)
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

func (ac *AutoscalerController) addDeployment(obj interface{}) {
	ac.HandlerAddEvents(obj)
}

func (ac *AutoscalerController) updateDeployment(old, cur interface{}) {
	ac.HandlerUpdateEvents(old, cur)
}

func (ac *AutoscalerController) deleteDeployment(obj interface{}) {
	ac.HandlerDeleteEvents(obj)
}

func (ac *AutoscalerController) addStatefulset(obj interface{}) {
	ac.HandlerAddEvents(obj)
}

func (ac *AutoscalerController) updateStatefulset(old, cur interface{}) {
	ac.HandlerUpdateEvents(old, cur)
}

func (ac *AutoscalerController) deleteStatefulset(obj interface{}) {
	ac.HandlerDeleteEvents(obj)
}

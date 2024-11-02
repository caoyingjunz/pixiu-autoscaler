/*
Copyright 2021 The Pixiu Authors.

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
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	appsinformers "k8s.io/client-go/informers/apps/v1"
	autoscalinginformers "k8s.io/client-go/informers/autoscaling/v2"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	appslisters "k8s.io/client-go/listers/apps/v1"
	autoscalinglisters "k8s.io/client-go/listers/autoscaling/v2"
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

// AutoscalerController is responsible for synchronizing HPA objects stored
// in the system.
type AutoscalerController struct {
	client        clientset.Interface
	eventRecorder record.EventRecorder

	syncHandler       func(hpaObj *autoscalingv2.HorizontalPodAutoscaler, event controller.Event) error
	enqueueAutoscaler func(hpaSpec controller.PixiuHpaSpec)

	syncHandler1      func(dKey string) error
	enqueueDeployment func(deployment *appsv1.Deployment)

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

	// Store and returns a reference to an empty store.
	items map[string]controller.Empty
}

// NewAutoscalerController creates a new AutoscalerController.
func NewAutoscalerController(
	dInformer appsinformers.DeploymentInformer,
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
		client:        client,
		eventRecorder: eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "pixiu-autoscaler"}),
		queue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "pixiu"),
		items:         controller.NewItems(),
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

	ac.syncHandler1 = ac.syncAutoscalers1
	ac.enqueueDeployment = ac.enqueue1

	ac.dListerSynced = dInformer.Informer().HasSynced
	ac.hpaListerSynced = hpaInformer.Informer().HasSynced

	return ac, nil
}

// Run begins watching and syncing.
func (ac *AutoscalerController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer ac.queue.ShutDown()

	klog.Infof("Starting Pixiu Autoscaler Controller")
	defer klog.Infof("Shutting down Pixiu Autoscaler Controller")

	// Wait for all involved caches to be synced, before processing items from the queue is started
	if !cache.WaitForNamedCacheSync("pixiu-autoscaler-controller", stopCh, ac.dListerSynced, ac.hpaListerSynced) {
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(ac.worker, time.Second, stopCh)
	}

	<-stopCh
}

// To ensure whether we need to maintain the HPA
func (ac *AutoscalerController) isHorizontalPodAutoscalerOwner(annotations map[string]string) bool {
	if annotations == nil {
		return false
	}

	var fdTarget, fdReplicas bool
	for anno := range annotations {
		if !fdReplicas {
			if anno == controller.MaxReplicas {
				fdReplicas = true
			}
		}
		if !fdTarget {
			_, found := ac.items[anno]
			if found {
				fdTarget = true
			}
		}

		if fdReplicas && fdTarget {
			return true
		}
	}

	return fdReplicas && fdTarget
}

func (ac *AutoscalerController) syncAutoscalers1(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.ErrorS(err, "Failed to split meta namespace cache key", "cacheKey", key)
		return err
	}

	startTime := time.Now()
	fmt.Println(namespace, name, startTime)
	return nil
}

// syncAutoscaler will sync the autoscaler with the given key.
// This function is not meant to be invoked concurrently with the same key.
func (ac *AutoscalerController) syncAutoscalers(hpa *autoscalingv2.HorizontalPodAutoscaler, event controller.Event) error {
	startTime := time.Now()
	klog.V(4).InfoS("Started syncing pixiu autoscaler", "pixiuautoscaler", "startTime", startTime)
	defer func() {
		klog.V(4).InfoS("Finished syncing pixiu autoscaler", "pixiuautoscaler", "duration", time.Since(startTime))
	}()

	var err error

	switch event {
	case controller.Add:
		_, err = ac.hpaLister.HorizontalPodAutoscalers(hpa.Namespace).Get(hpa.Name)
		if err == nil {
			// Since the hpa already exists, we should try to updated it.
			ac.queue.Add(controller.PixiuHpaSpec{
				Event: controller.Update,
				Hpa:   hpa,
			})
			return nil
		}

		if _, err = ac.client.AutoscalingV2().HorizontalPodAutoscalers(hpa.Namespace).Create(context.TODO(), hpa, metav1.CreateOptions{}); err != nil {
			if errors.IsAlreadyExists(err) {
				ac.queue.Add(controller.PixiuHpaSpec{
					Event: controller.Update,
					Hpa:   hpa,
				})
				return nil
			}

			ac.eventRecorder.Eventf(hpa, v1.EventTypeWarning, "FailedCreateHPA", fmt.Sprintf("Failed to create HPA %s/%s: %v", hpa.Namespace, hpa.Name, err))
			return err
		}
		ac.eventRecorder.Eventf(hpa, v1.EventTypeNormal, "CreateHPA", fmt.Sprintf("Create HPA %s/%s success", hpa.Namespace, hpa.Name))
	case controller.Update:
		var annotations map[string]string
		var uid types.UID

		kind := hpa.Spec.ScaleTargetRef.Kind
		switch kind {
		case controller.Deployment:
			d, err := ac.dLister.Deployments(hpa.Namespace).Get(hpa.Name)
			if err != nil {
				if errors.IsNotFound(err) {
					ac.queue.Add(controller.PixiuHpaSpec{
						Event: controller.Delete,
						Hpa:   hpa,
					})
				}

				return err
			}
			// check
			if !controller.IsOwnerReference(d.UID, hpa.OwnerReferences) {
				return nil
			}

			uid = d.UID
			annotations = d.Annotations
		}
		if !ac.isHorizontalPodAutoscalerOwner(annotations) {
			return nil
		}

		newHpa, err := controller.CreateHorizontalPodAutoscaler(hpa.Name, hpa.Namespace, uid, controller.AppsAPIVersion, kind, annotations)
		if err != nil {
			ac.eventRecorder.Eventf(hpa, v1.EventTypeWarning, "FailedNewestHPA", fmt.Sprintf("Failed extract newest HPA %s/%s", hpa.Namespace, hpa.Name))
			return err
		}

		curHpa, err := ac.hpaLister.HorizontalPodAutoscalers(hpa.Namespace).Get(hpa.Name)
		if err != nil {
			if !errors.IsNotFound(err) {
				ac.eventRecorder.Eventf(hpa, v1.EventTypeWarning, "FailedGetHPA", fmt.Sprintf("Failed to get current HPA %s/%s", hpa.Namespace, hpa.Name))
				return err
			}

			// Since the hpa not exists, we should try to add it.
			ac.queue.Add(controller.PixiuHpaSpec{
				Event: controller.Add,
				Hpa:   newHpa,
			})
			return nil
		}

		// To ensure whether need to updated the hpa
		if reflect.DeepEqual(curHpa.Spec, newHpa.Spec) {
			klog.V(0).Infof("HPA: %s/%s spec is not changed, no need to updated", hpa.Namespace, hpa.Name)
			return nil
		}

		if _, err = ac.client.AutoscalingV2().HorizontalPodAutoscalers(newHpa.Namespace).Update(context.TODO(), newHpa, metav1.UpdateOptions{}); err != nil {
			if !errors.IsNotFound(err) {
				ac.eventRecorder.Eventf(hpa, v1.EventTypeWarning, "FailedUpdateHPA", fmt.Sprintf("Failed to Recover update HPA %s/%s", hpa.Namespace, hpa.Name))
				return err

				// Since the hpa not exists, we should try to add it.
				ac.queue.Add(controller.PixiuHpaSpec{
					Event: controller.Add,
					Hpa:   newHpa,
				})
			}

			return nil
		}

		ac.eventRecorder.Eventf(hpa, v1.EventTypeNormal, "UpdateHPA", fmt.Sprintf("Update HPA %s/%ssuccess", hpa.Namespace, hpa.Name))
	case controller.Delete:
		if err = ac.client.AutoscalingV2beta2().HorizontalPodAutoscalers(hpa.Namespace).Delete(context.TODO(), hpa.Name, metav1.DeleteOptions{}); err != nil {
			if !errors.IsNotFound(err) {
				ac.eventRecorder.Eventf(hpa, v1.EventTypeWarning, "FailedDeleteHPA", fmt.Sprintf("Failed to delete HPA %s/%s", hpa.Namespace, hpa.Name))
				return err
			}

			ac.eventRecorder.Eventf(hpa, v1.EventTypeWarning, "DeleteNotExistHPA", fmt.Sprintf("Create exist HPA %s/%s", hpa.Namespace, hpa.Name))
			return nil
		}

		ac.eventRecorder.Eventf(hpa, v1.EventTypeNormal, "DeleteHPA", fmt.Sprintf("Delete HPA %s/%s success", hpa.Namespace, hpa.Name))
	}

	return nil
}

func (ac *AutoscalerController) enqueue(hpaObj controller.PixiuHpaSpec) {
	ac.queue.Add(hpaObj)
}

func (ac *AutoscalerController) enqueue1(deployment *appsv1.Deployment) {
	key, err := controller.KeyFunc(deployment)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", deployment, err))
		return
	}

	ac.queue.Add(key)
}

func (ac *AutoscalerController) addHPA(obj interface{}) {
	hpa := obj.(*autoscalingv2.HorizontalPodAutoscaler)
	if !controller.ManageByPixiuController(hpa) {
		return
	}

	klog.V(0).Infof("Adding HPA(manager by pixiu) %s/%s", hpa.Namespace, hpa.Name)
	ac.queue.Add(controller.PixiuHpaSpec{
		Event: controller.Add,
		Hpa:   hpa,
	})
}

// updateHPA figures out what HPA(s) is updated and wake them up. old and cur must be *autoscalingv2.HorizontalPodAutoscaler types.
func (ac *AutoscalerController) updateHPA(old, cur interface{}) {
	oldHPA := old.(*autoscalingv2.HorizontalPodAutoscaler)
	curHPA := cur.(*autoscalingv2.HorizontalPodAutoscaler)

	// Periodic resync will send update events for all known HPAs.
	// Two different versions of the same HPA will always have different ResourceVersions.
	if oldHPA.ResourceVersion == curHPA.ResourceVersion {
		return
	}

	if !controller.ManageByPixiuController(oldHPA) && !controller.ManageByPixiuController(curHPA) {
		return
	}

	klog.V(0).Infof("Updating HPA %s/%s", oldHPA.Namespace, oldHPA.Name)
	ac.queue.Add(controller.PixiuHpaSpec{
		Event: controller.Update,
		Hpa:   curHPA,
	})
}

func (ac *AutoscalerController) deleteHPA(obj interface{}) {
	hpa := obj.(*autoscalingv2.HorizontalPodAutoscaler)
	// TODO: rename the manager func
	if !controller.ManageByPixiuController(hpa) {
		return
	}

	klog.V(0).Infof("Deleting HPA %s/%s", hpa.Namespace, hpa.Name)
	ac.queue.Add(controller.PixiuHpaSpec{
		Event: controller.Update,
		Hpa:   hpa,
	})
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

	hpaSpec := key.(controller.PixiuHpaSpec)

	err := ac.syncHandler(hpaSpec.Hpa, hpaSpec.Event)
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

// This functions just wrap Handler Deployment Events for improve the readability of codes
func (ac *AutoscalerController) addDeployment(obj interface{}) {
	d := obj.(*appsv1.Deployment)
	klog.V(4).InfoS("Adding deployment", "deployment", klog.KObj(d))
	ac.enqueueDeployment(d)
}

func (ac *AutoscalerController) updateDeployment(old, cur interface{}) {
	oldD := old.(*appsv1.Deployment)
	curD := cur.(*appsv1.Deployment)
	klog.V(4).InfoS("Updating deployment", "deployment", klog.KObj(oldD))

	ac.enqueueDeployment(curD)
}

func (ac *AutoscalerController) deleteDeployment(obj interface{}) {
	d, ok := obj.(*appsv1.Deployment)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		d, ok = tombstone.Obj.(*appsv1.Deployment)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a Deployment %#v", obj))
			return
		}
	}
	klog.V(4).InfoS("Deleting deployment", "deployment", klog.KObj(d))
	ac.enqueueDeployment(d)
}

// extractHPAForDeployment returns the deployment managed by the given deployment.
func (ac *AutoscalerController) extractHPAForDeployment(d *appsv1.Deployment) (*autoscalingv2.HorizontalPodAutoscaler, error) {
	if !ac.isHorizontalPodAutoscalerOwner(d.Annotations) {
		return nil, nil
	}

	return controller.CreateHorizontalPodAutoscaler(d.Name, d.Namespace, d.UID, controller.AppsAPIVersion, controller.Deployment, d.Annotations)
}

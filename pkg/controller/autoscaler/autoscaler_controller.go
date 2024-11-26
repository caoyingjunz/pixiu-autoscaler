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

	"gopkg.in/yaml.v2"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/api/core/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	appsinformers "k8s.io/client-go/informers/apps/v1"
	autoscalinginformers "k8s.io/client-go/informers/autoscaling/v2"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	appslisters "k8s.io/client-go/listers/apps/v1"
	autoscalinglisters "k8s.io/client-go/listers/autoscaling/v2"
	corelisters "k8s.io/client-go/listers/core/v1"
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

	syncHandler       func(dKey string) error
	enqueueDeployment func(deployment *appsv1.Deployment)

	// dLister can list/get deployments from the shared informer's store
	dLister appslisters.DeploymentLister
	// hpaLister is able to list/get HPAs from the shared informer's cache
	hpaLister autoscalinglisters.HorizontalPodAutoscalerLister
	cmLister  corelisters.ConfigMapLister

	// dListerSynced returns true if the Deployment store has been synced at least once.
	dListerSynced cache.InformerSynced
	// hpaListerSynced returns true if the HPA store has been synced at least once.
	hpaListerSynced cache.InformerSynced

	cmListerSynced cache.InformerSynced

	// AutoscalerController that need to be synced
	queue workqueue.RateLimitingInterface

	// Store and returns a reference to an empty store.
	items map[string]controller.Empty
}

// NewAutoscalerController creates a new AutoscalerController.
func NewAutoscalerController(
	dInformer appsinformers.DeploymentInformer,
	hpaInformer autoscalinginformers.HorizontalPodAutoscalerInformer,
	cmInformer coreinformers.ConfigMapInformer,
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

	// ConfigMap
	cmInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: ac.updateCM,
	})

	ac.dLister = dInformer.Lister()
	ac.hpaLister = hpaInformer.Lister()
	ac.cmLister = cmInformer.Lister()

	// syncAutoscalers
	ac.syncHandler = ac.syncAutoscalers
	ac.enqueueDeployment = ac.enqueue

	ac.dListerSynced = dInformer.Informer().HasSynced
	ac.hpaListerSynced = hpaInformer.Informer().HasSynced
	ac.cmListerSynced = cmInformer.Informer().HasSynced

	return ac, nil
}

// Run begins watching and syncing.
func (ac *AutoscalerController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer ac.queue.ShutDown()

	klog.Infof("Starting Pixiu Autoscaler Controller")
	defer klog.Infof("Shutting down Pixiu Autoscaler Controller")

	// Wait for all involved caches to be synced, before processing items from the queue is started
	if !cache.WaitForNamedCacheSync("pixiu-autoscaler-controller", stopCh, ac.dListerSynced, ac.hpaListerSynced, ac.cmListerSynced) {
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(ac.worker, time.Second, stopCh)
	}

	<-stopCh
}

// IsDeploymentControlHPA 判断 deployment 是否维护 HPA
func (ac *AutoscalerController) IsDeploymentControlHPA(d *appsv1.Deployment) bool {
	annotations := d.GetAnnotations()
	if annotations == nil {
		return false
	}

	for annotation := range annotations {
		_, found := ac.items[annotation]
		if found {
			return true
		}
	}

	return false
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
	klog.V(4).InfoS("Started syncing pixiu autoscaler", "pixiu-autoscaler", "startTime", startTime)
	defer func() {
		klog.V(4).InfoS("Finished syncing pixiu autoscaler", "pixiu-autoscaler", "duration", time.Since(startTime))
	}()

	deployment, err := ac.dLister.Deployments(namespace).Get(name)
	if errors.IsNotFound(err) {
		klog.V(2).InfoS("Deployment has been deleted", "deployment", klog.KRef(namespace, name))
		return nil
	}
	if err != nil {
		return err
	}

	// 深拷贝，避免缓存被修改
	d := deployment.DeepCopy()
	if d.DeletionTimestamp != nil {
		return nil
	}

	hpaList, err := ac.getHPAsForDeployment(d)
	if err != nil {
		return err
	}
	return ac.sync(d, hpaList)
}

func (ac *AutoscalerController) fetchPrometheusAdapterConfig(d *appsv1.Deployment) (*v1.ConfigMap, error) {
	cms, err := ac.cmLister.ConfigMaps("").List(labels.Everything())
	if err != nil {
		ac.eventRecorder.Eventf(d, v1.EventTypeWarning, "FailedListCM", fmt.Sprintf("Failed extract list CM"))
		return nil, err
	}
	// 遍历列表，找到特定的 ConfigMap
	var targetConfigMap *v1.ConfigMap
	for _, cm := range cms {
		if cm.Name == "prometheus-adapter" {
			targetConfigMap = cm
			break
		}
	}
	if targetConfigMap == nil {
		ac.eventRecorder.Eventf(d, v1.EventTypeWarning, "FailedGetCM", fmt.Sprintf("Failed extract get CM %s", "prometheus-adapter"))
		return nil, err
	}
	return targetConfigMap, nil
}

func (ac *AutoscalerController) restartPrometheusAdapterDeployment() error {
	// 获取所有 Deployments
	deployments, err := ac.dLister.Deployments("").List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to get deployments : %v", err)
	}

	// 遍历 Deployments，找到 prometheus-adapter 部署
	var deployment *appsv1.Deployment
	for _, dep := range deployments {
		if dep.Name == "prometheus-adapter" {
			deployment = dep
			break
		}
	}

	// 如果没有找到 prometheus-adapter 部署，返回错误
	if deployment == nil {
		return fmt.Errorf("prometheus-adapter deployment not found")
	}

	// 确保 Annotations 不为 nil
	if deployment.Spec.Template.Annotations == nil {
		deployment.Spec.Template.Annotations = make(map[string]string)
	}

	// 设置重启时间
	deployment.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = metav1.Now().Format("2006-01-02T15:04:05Z")

	// 更新部署
	_, err = ac.client.AppsV1().Deployments(deployment.Namespace).Update(context.TODO(), deployment, metav1.UpdateOptions{})
	return err
}

func (ac *AutoscalerController) sync(d *appsv1.Deployment, hpaList []*autoscalingv2.HorizontalPodAutoscaler) error {
	// 1. deployment 存在，但是 hpa 注释不存在 => 移除已存在的 hpa
	if !ac.IsDeploymentControlHPA(d) {
		return ac.deleteHPAsInBatch(hpaList)
	}

	newHPA, err := controller.CreateHPAFromDeployment(d)
	if err != nil {
		ac.eventRecorder.Eventf(d, v1.EventTypeWarning, "FailedNewestHPA", fmt.Sprintf("Failed extract newest HPA %s/%s", d.GetNamespace(), d.GetName()))
		return err
	}
	if newHPA.Spec.Metrics[0].External != nil {
		//获取ConfigMap
		configMap, err := ac.fetchPrometheusAdapterConfig(d)
		if err != nil {
			return err
		}
		var config controller.PrometheusAdapterConfig
		err = yaml.Unmarshal([]byte(configMap.Data["config.yaml"]), &config)
		if err != nil {
			klog.Errorf("Failed to unmarshal Prometheus adapter config: %v", err)
			return err
		}
		exists := false
		for _, v := range config.ExternalRules {
			if v.SeriesQuery == newHPA.Spec.Metrics[0].External.Metric.Name {
				exists = true
				break
			}
		}
		if !exists {
			newRule := controller.ExternalRule{
				MetricsQuery: "<<.Series>>",
				Name: controller.RuleName{
					As:      "",
					Matches: "",
				},
				Resources: controller.ResourceMap{
					Overrides: map[string]controller.ResourceOverride{
						"namespace": {Resource: "namespace"},
					},
				},
				SeriesQuery: newHPA.Spec.Metrics[0].External.Metric.Name,
			}
			config.ExternalRules = append(config.ExternalRules, newRule)
			updatedYaml, err := yaml.Marshal(&config)
			if err != nil {
				klog.Errorf("Failed to marshal Prometheus adapter config: %v", err)
				return err
			}
			// 更新 ConfigMap
			configMap.Data["config.yaml"] = string(updatedYaml)
			configMap.Annotations["skip-reconcile"] = "true"
			_, err = ac.client.CoreV1().ConfigMaps(configMap.Namespace).Update(context.TODO(), configMap, metav1.UpdateOptions{})
			if err != nil {
				ac.eventRecorder.Eventf(d, v1.EventTypeWarning, "FailedSetCM", fmt.Sprintf("Failed extract set CM %s/%s", configMap.Namespace, "prometheus-adapter"))
				return err
			}
			//err = ac.restartPrometheusAdapterDeployment()
			if err != nil {
				klog.ErrorS(err, "Failed to restart Prometheus adapter deployment")
				return err
			}

			klog.Infof("新规则已添加到 ExternalRules 并更新到 ConfigMap")
			klog.Infof("Deployment %s/%s 已成功重启\n", configMap.Namespace, "prometheus-adapter")
		} else {
			klog.Infof("规则已存在，跳过添加")
		}
	}

	if len(hpaList) == 0 {
		// 新建
		_, err = ac.client.AutoscalingV2().HorizontalPodAutoscalers(newHPA.Namespace).Create(context.TODO(), newHPA, metav1.CreateOptions{})
		if err != nil {
			ac.eventRecorder.Eventf(newHPA, v1.EventTypeWarning, "FailedCreateHPA", fmt.Sprintf("Failed to create HPA %s/%s: %v", newHPA.Namespace, newHPA.Name, err))
			return err
		}
		ac.eventRecorder.Eventf(newHPA, v1.EventTypeNormal, "CreateHPA", fmt.Sprintf("Create HPA %s/%s success", newHPA.Namespace, newHPA.Name))
	} else {
		// 更新 if necessary
		oldHPA := hpaList[0]
		if err := ac.deleteHPAsInBatch(hpaList[1:]); err != nil {
			return err
		}

		if reflect.DeepEqual(oldHPA.Spec, newHPA.Spec) {
			klog.V(2).Infof("HPA: %s/%s is not changed", newHPA.Namespace, newHPA.Name)
			return nil
		}
		if oldHPA.Spec.Metrics[0].External != nil {
			hpaList, err := ac.client.AutoscalingV2().HorizontalPodAutoscalers("").List(context.TODO(), metav1.ListOptions{})
			if err != nil {
				return err
			}
			// 定义一个 map 来存储 hpaList 中 External.Metric.Name 出现的次数
			metricNameCount := make(map[string]int)

			// 遍历 hpaList，将 Metric.Name 的出现次数统计到 map 中
			for _, hpa := range hpaList.Items {
				if len(hpa.Spec.Metrics) > 0 && hpa.Spec.Metrics[0].External != nil {
					metricName := hpa.Spec.Metrics[0].External.Metric.Name
					metricNameCount[metricName]++
				}
			}

			// 检查 oldHPA.Spec.Metrics 是否有值，并判断是否需要处理
			if len(oldHPA.Spec.Metrics) > 0 && oldHPA.Spec.Metrics[0].External != nil {
				metricName := oldHPA.Spec.Metrics[0].External.Metric.Name
				if count, exists := metricNameCount[metricName]; exists && count == 1 {
					// 获取 Prometheus Adapter 的 ConfigMap
					configMap, err := ac.fetchPrometheusAdapterConfig(d)
					if err != nil {
						return err
					}

					// 解析 ConfigMap 中的 ExternalRules
					var config controller.PrometheusAdapterConfig
					err = yaml.Unmarshal([]byte(configMap.Data["config.yaml"]), &config)
					if err != nil {
						klog.Errorf("Failed to unmarshal Prometheus adapter config: %v", err)
						return err
					}

					// 过滤掉与 oldHPA.Spec.Metrics[0].External.Metric.Name 匹配的规则
					var updatedRules []controller.ExternalRule
					exists := false
					for _, rule := range config.ExternalRules {
						if rule.SeriesQuery != metricName {
							updatedRules = append(updatedRules, rule)
						} else {
							exists = true
							klog.Infof("Removed rule with SeriesQuery: %s", rule.SeriesQuery)
						}
					}
					if exists {

						config.ExternalRules = updatedRules

						// 更新 ConfigMap
						updatedYaml, err := yaml.Marshal(&config)
						if err != nil {
							klog.Errorf("Failed to marshal updated Prometheus adapter config: %v", err)
							return err
						}
						configMap.Data["config.yaml"] = string(updatedYaml)
						_, err = ac.fetchPrometheusAdapterConfig(d)
						if err != nil {
							klog.Errorf("Failed to update ConfigMap %s/%s: %v", configMap.Namespace, configMap.Name, err)
							return err
						}
						err = ac.restartPrometheusAdapterDeployment()
						if err != nil {
							return err
						}
						klog.Infof("ConfigMap %s/%s updated successfully, rule with SeriesQuery %s removed", configMap.Namespace, configMap.Name, metricName)
						klog.Infof("Deployment %s/%s 已成功重启\n", configMap.Namespace, "prometheus-adapter")
					}
				}
			}

		}
		if _, err = ac.client.AutoscalingV2().HorizontalPodAutoscalers(newHPA.Namespace).Update(context.TODO(), newHPA, metav1.UpdateOptions{}); err != nil {
			if !errors.IsNotFound(err) {
				ac.eventRecorder.Eventf(newHPA, v1.EventTypeWarning, "FailedUpdateHPA", fmt.Sprintf("Failed to Recover update HPA %s/%s", newHPA.Namespace, newHPA.Name))
				klog.Errorf("Failed to update HPA %s/%s %v", newHPA.Namespace, newHPA.Name, err)
				return err
			}
		}
		ac.eventRecorder.Eventf(newHPA, v1.EventTypeNormal, "UpdateHPA", fmt.Sprintf("Update HPA %s/%s success", newHPA.Namespace, newHPA.Name))
	}

	return nil
}

func (ac *AutoscalerController) deleteHPAsInBatch(hpaList []*autoscalingv2.HorizontalPodAutoscaler) error {
	if len(hpaList) == 0 {
		return nil
	}
	for _, hpa := range hpaList {
		if err := ac.client.AutoscalingV2().HorizontalPodAutoscalers(hpa.Namespace).Delete(context.TODO(), hpa.Name, metav1.DeleteOptions{}); err != nil {
			if !errors.IsNotFound(err) {
				ac.eventRecorder.Eventf(hpa, v1.EventTypeWarning, "FailedDeleteHPA", fmt.Sprintf("Failed to delete HPA %s/%s", hpa.Namespace, hpa.Name))
				return err
			}
		}
		ac.eventRecorder.Eventf(hpa, v1.EventTypeNormal, "DeleteHPA", fmt.Sprintf("Delete HPA %s/%s", hpa.Namespace, hpa.Name))
	}

	return nil
}

func (ac *AutoscalerController) getHPAsForDeployment(d *appsv1.Deployment) ([]*autoscalingv2.HorizontalPodAutoscaler, error) {
	hpaList, err := ac.hpaLister.HorizontalPodAutoscalers(d.Namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}

	var wanted []*autoscalingv2.HorizontalPodAutoscaler
	for _, hpa := range hpaList {
		controllerRef := metav1.GetControllerOf(hpa)
		if controllerRef == nil {
			continue
		}
		if d.UID == controllerRef.UID && controllerRef.Kind == controller.Deployment && controllerRef.Name == d.Name {
			wanted = append(wanted, hpa)
		}
	}

	return wanted, nil
}

func (ac *AutoscalerController) enqueue(deployment *appsv1.Deployment) {
	key, err := controller.KeyFunc(deployment)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", deployment, err))
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

// This functions just wrap Handler Deployment Events for improve the readability of codes
func (ac *AutoscalerController) addDeployment(obj interface{}) {
	d := obj.(*appsv1.Deployment)
	klog.V(4).InfoS("Adding deployment", "deployment", klog.KObj(d))
	ac.enqueueDeployment(d)
}

func (ac *AutoscalerController) updateDeployment(old, cur interface{}) {
	oldD := old.(*appsv1.Deployment)
	curD := cur.(*appsv1.Deployment)

	// Two different versions of the same HPA will always have different ResourceVersions.
	if oldD.ResourceVersion == curD.ResourceVersion {
		return
	}
	// deployment 的注释未变化，则HPA不变
	if reflect.DeepEqual(oldD.Annotations, curD.Annotations) {
		return
	}
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

func (ac *AutoscalerController) addHPA(obj interface{}) {
	hpa := obj.(*autoscalingv2.HorizontalPodAutoscaler)

	if hpa.DeletionTimestamp != nil {
		// 重启控制器会中断原先删除过程，存在删除标识则继续删除
		ac.deleteHPA(hpa)
		return
	}

	// 如果存在 OwnerReference， 则直接获取上级资源
	if controllerRef := metav1.GetControllerOf(hpa); controllerRef != nil {
		d := ac.resolveControllerRef(hpa.Namespace, controllerRef)
		if d == nil {
			return
		}
		klog.V(4).InfoS("HPA added", "hpa", klog.KObj(hpa))
		ac.enqueueDeployment(d)
		return
	}
}

// updateHPA figures out what HPA(s) is updated and wake them up. old and cur must be *autoscalingv2.HorizontalPodAutoscaler types.
func (ac *AutoscalerController) updateHPA(old, cur interface{}) {
	oldHPA := old.(*autoscalingv2.HorizontalPodAutoscaler)
	curHPA := cur.(*autoscalingv2.HorizontalPodAutoscaler)

	// Two different versions of the same HPA will always have different ResourceVersions.
	if oldHPA.ResourceVersion == curHPA.ResourceVersion {
		return
	}

	curControllerRef := metav1.GetControllerOf(curHPA)
	oldControllerRef := metav1.GetControllerOf(oldHPA)
	controllerRefChanged := !reflect.DeepEqual(curControllerRef, oldControllerRef)
	if controllerRefChanged && oldControllerRef != nil {
		// hpa 的 ControllerRef 发生了变化，同步老的 controller
		if d := ac.resolveControllerRef(oldHPA.Namespace, oldControllerRef); d != nil {
			ac.enqueueDeployment(d)
		}
	}

	if curControllerRef != nil {
		if d := ac.resolveControllerRef(curHPA.Namespace, curControllerRef); d != nil {
			ac.enqueueDeployment(d)
		}
	}
}

func (ac *AutoscalerController) deleteHPA(obj interface{}) {
	hpa, ok := obj.(*autoscalingv2.HorizontalPodAutoscaler)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		hpa, ok = tombstone.Obj.(*autoscalingv2.HorizontalPodAutoscaler)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a HorizontalPodAutoscaler %#v", obj))
			return
		}
	}

	controllerRef := metav1.GetControllerOf(hpa)
	if controllerRef == nil {
		return
	}
	d := ac.resolveControllerRef(hpa.Namespace, controllerRef)
	if d == nil {
		return
	}
	klog.V(0).Infof("Deleting HPA %s/%s", hpa.Namespace, hpa.Name)
	ac.enqueueDeployment(d)
}

func (ac *AutoscalerController) updateCM(old, cur interface{}) {
	oldCM := old.(*corev1.ConfigMap)
	curCM := cur.(*corev1.ConfigMap)

	// 如果不是prometheus-adapter配置文件修改就返回
	if oldCM.Name != "prometheus-adapter" || curCM.Name != "prometheus-adapter" {
		return
	}

	// 获取所有的 Deployment 列表
	deployments, err := ac.dLister.Deployments("").List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list Deployments: %v", err)
		return
	}

	// 遍历 Deployment，查找带有特定注解的对象
	for _, deployment := range deployments {
		if skipReconcile, _ := deployment.GetAnnotations()["skip-reconcile"]; skipReconcile == "true" {
			continue
		}
		if annotation, exists := deployment.GetAnnotations()["hpa.caoyingjunz.io/targetCustomMetric"]; exists && annotation != "" {
			key, err := cache.MetaNamespaceKeyFunc(deployment)
			if err != nil {
				klog.Errorf("Failed to generate key for Deployment: %v", err)
				continue
			}
			// 将 Deployment 加入队列以进行重新同步
			ac.queue.Add(key)
			klog.Infof("Deployment %s added to workqueue for resync", deployment.Name)
		}
	}
}

func (ac *AutoscalerController) resolveControllerRef(namespace string, controllerRef *metav1.OwnerReference) *appsv1.Deployment {
	if controllerRef.Kind != controller.Deployment {
		return nil
	}
	d, err := ac.dLister.Deployments(namespace).Get(controllerRef.Name)
	if err != nil {
		return nil
	}
	if d.UID != controllerRef.UID {
		return nil
	}
	return d
}

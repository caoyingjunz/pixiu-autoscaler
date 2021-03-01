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

package handlers

import (
	"context"
	"fmt"
	"strconv"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2beta2"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilpointer "k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func NewHPAHandler(client client.Client, log logr.Logger) *HPAHandler {
	kubezas := KubezAutoscaler{}
	kubezas.init(minReplicas, maxReplicas, targetCPUUtilizationPercentage)

	return &HPAHandler{
		client: client,
		log:    log,
		kas:    kubezas,
	}
}

type HPAHandler struct {
	client client.Client
	log    logr.Logger
	kas    KubezAutoscaler
}

func (h *HPAHandler) HandlerAutoscaler(ctx context.Context, namespacedName types.NamespacedName, handlerResource interface{}, scaleTarget ScaleTarget) error {
	isHpa := true
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	err := h.client.Get(context.TODO(), namespacedName, hpa)
	if err != nil {
		if !errors.IsNotFound(err) {
			h.log.Error(err, "HandlerAutoscaler")
			// Error reading the object - requeue the request.
			return err
		}
		isHpa = false
	}

	var isResource bool
	switch scaleTarget {
	case Deployment:
		deployment := handlerResource.(*appsv1.Deployment)
		if len(deployment.Name) != 0 {
			isResource = true
		}

		if !isResource && !isHpa {
			// deployment 和 hpa 均不存在，不需要做任何修改，直接返回
			return nil
		}

		if !isResource && isHpa {
			// deployment 不存在，但是 hpa 存在，
			// 检查 hpa 是否为 kubez 所创建，如果是，则删除 hpa
			_, ok := hpa.Labels[KubezHpaController]
			if ok {
				h.log.Info("deployment " + namespacedName.String() + " deleted and the hpa is deleting")
				return h.client.Delete(context.TODO(), hpa)
			}
		}

		// 获取 hpa 所需要的参数
		minRcs, minExist := deployment.Annotations[minReplicas]
		maxRcs, maxExist := deployment.Annotations[maxReplicas]
		targetCPU, cpuExist := deployment.Annotations[targetCPUUtilizationPercentage]

		var minInt32, maxInt32, targetCPUInt32 int32
		if minExist {
			minRcsInt, err := strconv.ParseInt(minRcs, 10, 32)
			if err != nil {
				h.log.Error(err, "strconv.ParseInt")
				return err
			}
			minInt32 = int32(minRcsInt)
		}
		if maxExist {
			maxRcsInt, err := strconv.ParseInt(maxRcs, 10, 32)
			if err != nil {
				h.log.Error(err, "strconv.ParseInt")
				return err
			}
			maxInt32 = int32(maxRcsInt)
		}

		// targetCPUUtilizationPercentage
		if cpuExist {
			targetInt, err := strconv.ParseInt(targetCPU, 10, 32)
			if err != nil {
				return err
			}
			targetCPUInt32 = int32(targetInt)
		}
		if targetCPUInt32 > 100 || targetCPUInt32 < 0 {
			return fmt.Errorf("targetCPUUtilizationPercentage range must be 0 through 100")
		}
		if targetCPUInt32 == 0 {
			targetCPUInt32 = 80
		}

		if isResource && !isHpa {
			// deployment 存在，但是 hpa 不存在
			// 检查 deployment 是否需要创建 hpa，如果是，则创建 hpa
			// TODO: 优化
			if minExist && maxExist {
				// 只有 2 个参数均存在的时候，才会触发 hpa 的创建
				hpaAnnotations := map[string]int32{
					minReplicas:                    minInt32,
					maxReplicas:                    maxInt32,
					targetCPUUtilizationPercentage: targetCPUInt32,
				}
				h.log.Info("deployment " + namespacedName.String() + " updated and the hpa is creating")
				hpa := createHorizontalPodAutoscaler(namespacedName, deployment.UID, deployment.APIVersion, deployment.Kind, hpaAnnotations)
				return h.client.Create(context.TODO(), hpa)
			}
		}

		if isResource && isHpa {
			// deployment 存在，hpa 注释不存在，且 hpa 存在，删除
			if minInt32 == 0 && maxInt32 == 0 {
				h.log.Info("deployment " + namespacedName.String() + " updated and the hpa is deleting")
				return h.client.Delete(context.TODO(), hpa)
			}

			// deployment 和 hpa 均存在，检查是否有变化，如果有则更新
			// TODO: 需要优化
			if minInt32 != *hpa.Spec.MinReplicas || maxInt32 != hpa.Spec.MaxReplicas {
				hpa.Spec.MinReplicas = utilpointer.Int32Ptr(minInt32)
				hpa.Spec.MaxReplicas = maxInt32
				h.log.Info("deployment " + namespacedName.String() + " updated and the hpa is updating")
				return h.client.Update(context.TODO(), hpa)
			}
		}
	}

	return nil
}

func (h *HPAHandler) ReconcileAutoscaler(ctx context.Context, namespacedName types.NamespacedName) error {
	// We assume the hpa is exists at begin
	isHpaExists := true

	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	err := h.client.Get(context.TODO(), namespacedName, hpa)
	if err != nil {
		if !errors.IsNotFound(err) {
			h.log.Error(err, "ReconcileAutoscaler, Get HPA failed")
			// Error reading the object - requeue the request.
			return err
		}
		isHpaExists = false
	}

	if isHpaExists {
		ScaleTargetKind := hpa.Spec.ScaleTargetRef.Kind
		switch ScaleTargetKind {
		case "Deployment":
			deployment := &appsv1.Deployment{}
			err := h.client.Get(context.TODO(), namespacedName, deployment)
			if err != nil {
				if !errors.IsNotFound(err) {
					h.log.Error(err, "ReconcileAutoscaler, Get Deployment failed")
					return err
				}
				// TODO: 如果 deployment 需要删除 hpa
				return nil
			}
			kubezAnnotations, err := parseKubezAutoscaler(deployment.Annotations)
			if err != nil {
				h.log.Error(err, "parseKubezAutoscaler")
				return err
			}

			minRcs := kubezAnnotations[minReplicas]
			maxRcs := kubezAnnotations[maxReplicas]
			if *hpa.Spec.MinReplicas != minRcs || hpa.Spec.MaxReplicas != maxRcs {
				hpa.Spec.MinReplicas = utilpointer.Int32Ptr(minRcs)
				hpa.Spec.MaxReplicas = maxRcs
				h.log.Info("The HPA " + namespacedName.String() + " is updating")
				return h.client.Update(context.TODO(), hpa)
			}
		}
	} else {
		// If HPA deleted, try fetch the HPA from Deployment or the other resources
		// TODO: for now, Just fetch from deployments
		isDeploymentExists := true
		deployment := &appsv1.Deployment{}
		err := h.client.Get(context.TODO(), namespacedName, deployment)
		if err != nil {
			if !errors.IsNotFound(err) {
				h.log.Error(err, "ReconcileAutoscaler, Get Deployment failed")
				return err
			}
			isDeploymentExists = false
		}

		if isDeploymentExists && needToBeRecover(deployment.Annotations) {
			kubezAnnotations, err := parseKubezAutoscaler(deployment.Annotations)
			if err != nil {
				h.log.Error(err, "parseKubezAutoscaler")
				return err
			}
			hpa := createHorizontalPodAutoscaler(namespacedName, deployment.UID, deployment.APIVersion, deployment.Kind, kubezAnnotations)
			h.log.Info("The HPA " + namespacedName.String() + " is recovering")
			return h.client.Create(context.TODO(), hpa)
		}
	}

	return nil
}

func createHorizontalPodAutoscaler(namespacedName types.NamespacedName, uid types.UID, apiVersion, kind string, hpaAnnotations map[string]int32) *autoscalingv2.HorizontalPodAutoscaler {

	controller := true
	blockOwnerDeletion := true
	ownerReference := metav1.OwnerReference{
		APIVersion:         apiVersion,
		Kind:               kind,
		Name:               namespacedName.Name,
		UID:                uid,
		Controller:         &controller,
		BlockOwnerDeletion: &blockOwnerDeletion,
	}

	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		TypeMeta: metav1.TypeMeta{
			Kind:       "HorizontalPodAutoscaler",
			APIVersion: "autoscaling/v2beta2",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      namespacedName.Name,
			Namespace: namespacedName.Namespace,
			Labels: map[string]string{
				KubezHpaController: KubezManger,
			},
			OwnerReferences: []metav1.OwnerReference{
				ownerReference,
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			MinReplicas: utilpointer.Int32Ptr(hpaAnnotations[minReplicas]),
			MaxReplicas: hpaAnnotations[maxReplicas],
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: apiVersion,
				Kind:       kind,
				Name:       namespacedName.Name,
			},
		},
	}

	// CPU metric
	metric := autoscalingv2.MetricSpec{
		Type: autoscalingv2.ResourceMetricSourceType,
		Resource: &autoscalingv2.ResourceMetricSource{
			Name: v1.ResourceCPU,
			Target: autoscalingv2.MetricTarget{
				Type:               autoscalingv2.UtilizationMetricType,
				AverageUtilization: utilpointer.Int32Ptr(hpaAnnotations[targetCPUUtilizationPercentage]),
			},
		},
	}

	hpa.Spec.Metrics = []autoscalingv2.MetricSpec{metric}
	return hpa
}

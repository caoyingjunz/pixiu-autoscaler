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
	"strconv"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/api/autoscaling/v2beta2"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

	hpa := &v2beta2.HorizontalPodAutoscaler{}
	err := h.client.Get(context.TODO(), namespacedName, hpa)
	if err != nil {
		if !errors.IsNotFound(err) {
			h.log.Error(err, "HandlerAutoscaler")
			// Error reading the object - requeue the request.
			return err
		}
	}

	var isHpa bool
	if len(hpa.Name) != 0 {
		isHpa = true
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
		//targetCPU, cpuExist := deployment.Annotations[targetCPUUtilizationPercentage]

		var minInt32, maxInt32 int32
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

		if isResource && !isHpa {
			// deployment 存在，但是 hpa 不存在
			// 检查 deployment 是否需要创建 hpa，如果是，则创建 hpa
			// TODO: 优化
			if minExist && maxExist {
				// 只有 2 个参数均存在的时候，才会触发 hpa 的创建
				hpaAnnotations := map[string]int32{
					minReplicas: minInt32,
					maxReplicas: maxInt32,
				}
				h.log.Info("deployment " + namespacedName.String() + " updated and the hpa is creating")
				return h.client.Create(context.TODO(), createHorizontalPodAutoscaler(namespacedName, deployment.APIVersion, deployment.Kind, hpaAnnotations))
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
				hpa.Spec.MinReplicas = &minInt32
				hpa.Spec.MaxReplicas = maxInt32
				h.log.Info("deployment " + namespacedName.String() + " updated and the hpa is updating")
				return h.client.Update(context.TODO(), hpa)
			}
		}
	}

	return nil
}

func createHorizontalPodAutoscaler(namespacedName types.NamespacedName, apiVersion, kind string, hpaAnnotations map[string]int32) *v2beta2.HorizontalPodAutoscaler {
	mrs := hpaAnnotations[minReplicas]
	hpa := &v2beta2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      namespacedName.Name,
			Namespace: namespacedName.Namespace,
			Labels: map[string]string{
				KubezHpaController: KubezManger,
			},
		},
		Spec: v2beta2.HorizontalPodAutoscalerSpec{
			MinReplicas: &mrs,
			MaxReplicas: hpaAnnotations[maxReplicas],
			ScaleTargetRef: v2beta2.CrossVersionObjectReference{
				APIVersion: apiVersion,
				Kind:       kind,
				Name:       namespacedName.Name,
			},
		},
	}
	return hpa
}

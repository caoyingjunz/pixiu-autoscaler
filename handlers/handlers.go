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
	"k8s.io/api/autoscaling/v2beta2"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func NewHPAHandler(client client.Client) *HPAHandler {
	kubezas := KubezAutoscaler{}
	kubezas.init(minReplicas, maxReplicas, targetCPUUtilizationPercentage)

	return &HPAHandler{
		client: client,
		kas:    kubezas,
	}
}

type HPAHandler struct {
	client client.Client
	kas    KubezAutoscaler
}

func (h *HPAHandler) HandlerAutoscaler(ctx context.Context, namespacedName types.NamespacedName, handlerType HandlerType, annotations map[string]string) error {

	hpa := &v2beta2.HorizontalPodAutoscaler{}
	err := h.client.Get(context.TODO(), namespacedName, hpa)
	if err == nil {
		if handlerType != Delete {
			handlerType = Update
		}
	} else {
		if !errors.IsNotFound(err) {
			if handlerType != Delete {
				handlerType = Create
			}
		} else {
			// TODO
			return nil
		}
	}

	switch handlerType {
	case Delete:
		// Delete HPA
		// TODO: 需要判断 hpa 是否属于 deployment
		if err := h.client.Delete(context.TODO(), hpa); err != nil {
			return nil
		}
	case Create:
		// Create HPA
		fmt.Println("create HPA")
		return nil

	case Update:
		// Update HPA
		fmt.Println("update HPA")
		return nil
	}

	hpaAnnotations := make(map[string]string)
	for k, v := range annotations {
		if h.kas.isKubezAnnotation(k) {
			hpaAnnotations[k] = v
		}
	}

	// let it go
	if len(hpaAnnotations) == 0 {
		// TODO
		return nil
	}

	if err := h.kas.isValid(hpaAnnotations); err != nil {
		return err
	}

	newHpa := createHorizontalPodAutoscaler(namespacedName, hpaAnnotations)
	err = h.client.Create(context.TODO(), newHpa)
	if err != nil {
		return err

	}
	return nil
}

func createHorizontalPodAutoscaler(namespacedName types.NamespacedName, hpaAnnotations map[string]string) *v2beta2.HorizontalPodAutoscaler {

	//minReplicas := int32(hpaAnnotations[minReplicas])
	minReplicas := int32(2)

	hpa := &v2beta2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      namespacedName.Name,
			Namespace: namespacedName.Namespace,
		},
		Spec: v2beta2.HorizontalPodAutoscalerSpec{
			MinReplicas: &minReplicas,
			MaxReplicas: int32(3),
			ScaleTargetRef: v2beta2.CrossVersionObjectReference{
				// TODO
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       namespacedName.Name,
			},
		},
	}
	return hpa
}

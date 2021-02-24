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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func NewHPAHandler(client client.Client) *HPAHandler {
	return &HPAHandler{
		client: client,
	}
}

type HPAHandler struct {
	client client.Client
}

func (h *HPAHandler) HandlerAutoscaler(ctx context.Context, namespacedName types.NamespacedName, annotations map[string]string) error {

	hpa := &v2beta2.HorizontalPodAutoscaler{}
	err := h.client.Get(ctx, namespacedName, hpa)
	if err != nil {
		if errors.IsNotFound(err) {
			fmt.Println("hpa not found")
		}
	}

	return nil
}

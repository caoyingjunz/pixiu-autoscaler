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

package controllers

import (
	"context"

	"github.com/go-logr/logr"
	autosv1 "k8s.io/api/autoscaling/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/caoyingjunz/kubez-autoscaler/handlers"
)

func NewHorizontalPodAutoscalerReconciler(client client.Client, log logr.Logger, scheme *runtime.Scheme, handler *handlers.HPAHandler) *HorizontalPodAutoscalerReconciler {
	return &HorizontalPodAutoscalerReconciler{
		client:  client,
		log:     log,
		scheme:  scheme,
		handler: handler,
	}
}

// HorizontalPodAutoscalerReconciler reconciles a HorizontalPodAutoscaler object
type HorizontalPodAutoscalerReconciler struct {
	client  client.Client
	log     logr.Logger
	scheme  *runtime.Scheme
	handler *handlers.HPAHandler
}

func (hp *HorizontalPodAutoscalerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = hp.log.WithValues("kubez", req.NamespacedName)

	hpa := &autosv1.HorizontalPodAutoscaler{}
	err := hp.client.Get(context.TODO(), req.NamespacedName, hpa)
	if err != nil {
		if !errors.IsNotFound(err) {
			// Error reading the object - requeue the request.
			hp.log.Error(err, "HorizontalPodAutoscalerReconciler")
			return ctrl.Result{}, err
		}
	}

	err := hp.handler.ReconcileAutoscaler()

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the HorizontalPodAutoscaler Manager.
func (hp *HorizontalPodAutoscalerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&autosv1.HorizontalPodAutoscaler{}).
		Complete(hp)
}

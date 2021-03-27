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

package controller

import (
	"fmt"
	"strconv"
	"strings"

	apps "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2beta2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientset "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"
	utilpointer "k8s.io/utils/pointer"
)

const (
	HorizontalPodAutoscaler           string = "HorizontalPodAutoscaler"
	HorizontalPodAutoscalerAPIVersion string = "autoscaling/v2beta2"
)

var (
	KeyFunc = cache.DeletionHandlingMetaNamespaceKeyFunc
)

type ControllerClientBuilder interface {
	Config(name string) (*restclient.Config, error)
	ConfigOrDie(name string) *restclient.Config
	Client(name string) (clientset.Interface, error)
	ClientOrDie(name string) clientset.Interface
}

// SimpleControllerClientBuilder returns a fixed client with different user agents
type SimpleControllerClientBuilder struct {
	// ClientConfig is a skeleton config to clone and use as the basis for each controller client
	ClientConfig *restclient.Config
}

func (b SimpleControllerClientBuilder) Config(name string) (*restclient.Config, error) {
	clientConfig := *b.ClientConfig
	return restclient.AddUserAgent(&clientConfig, name), nil
}

func (b SimpleControllerClientBuilder) ConfigOrDie(name string) *restclient.Config {
	clientConfig, err := b.Config(name)
	if err != nil {
		klog.Fatal(err)
	}
	return clientConfig
}

func (b SimpleControllerClientBuilder) Client(name string) (clientset.Interface, error) {
	clientConfig, err := b.Config(name)
	if err != nil {
		return nil, err
	}
	return clientset.NewForConfig(clientConfig)
}

func (b SimpleControllerClientBuilder) ClientOrDie(name string) clientset.Interface {
	client, err := b.Client(name)
	if err != nil {
		klog.Fatal(err)
	}
	return client
}

// AutoscalerContext is responsible for kubernetes resources stored.
type AutoscalerContext struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	APIVersion  string            `json:"api_version"`
	Kind        string            `json:"kind"`
	UID         types.UID         `json:"uid"`
	Annotations map[string]string `json:"annotations"`
}

// NewAutoscalerContext extracts contexts which we needed from kubernetes resouces.
// The resouces could be Deployment, StatefulSet for now
func NewAutoscalerContext(obj interface{}) *AutoscalerContext {
	// TODO: 后续优化，直接获取 hpa 的 Annotations
	switch o := obj.(type) {
	case *apps.Deployment:
		return &AutoscalerContext{
			Name:        o.Name,
			Namespace:   o.Namespace,
			APIVersion:  o.APIVersion,
			Kind:        "Deployment",
			UID:         o.UID,
			Annotations: o.Annotations,
		}
	case *apps.StatefulSet:
		return &AutoscalerContext{
			Name:        o.Name,
			Namespace:   o.Namespace,
			APIVersion:  o.APIVersion,
			Kind:        "StatefulSet",
			UID:         o.UID,
			Annotations: o.Annotations,
		}
	default:
		// never happens
		return nil
	}
}

func CreateHorizontalPodAutoscaler(
	name string,
	namespace string,
	uid types.UID,
	apiVersion string,
	kind string,
	annotations map[string]string) (*autoscalingv2.HorizontalPodAutoscaler, error) {

	minReplicas, err := ExtractReplicas(annotations, MinReplicas)
	if err != nil {
		klog.Errorf("Extract MinReplicas from annotations failed: %v", err)
		return nil, err
	}
	maxReplicas, err := ExtractReplicas(annotations, MaxReplicas)
	if err != nil {
		klog.Errorf("Extract maxReplicas from annotations failed: %v", err)
		return nil, err
	}
	metrics, err := parseMetrics(annotations)
	if err != nil {
		klog.Errorf("Parse metrics from annotations failed: %v", err)
		return nil, fmt.Errorf("Parse metrics from annotations failed: %v", err)
	}
	klog.Infof("Parse %d metrics from annotations for %s/%s", len(metrics), namespace, name)

	controller := true
	blockOwnerDeletion := true
	ownerReference := metav1.OwnerReference{
		APIVersion:         apiVersion,
		Kind:               kind,
		Name:               name,
		UID:                uid,
		Controller:         &controller,
		BlockOwnerDeletion: &blockOwnerDeletion,
	}

	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		TypeMeta: metav1.TypeMeta{
			Kind:       HorizontalPodAutoscaler,
			APIVersion: HorizontalPodAutoscalerAPIVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{
				ownerReference,
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			MinReplicas: utilpointer.Int32Ptr(minReplicas),
			MaxReplicas: maxReplicas,
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: apiVersion,
				Kind:       kind,
				Name:       name,
			},
			Metrics: metrics,
		},
	}

	return hpa, nil
}

func parseMetrics(annotations map[string]string) ([]autoscalingv2.MetricSpec, error) {
	metrics := make([]autoscalingv2.MetricSpec, 0)

	for metricName, metricValue := range annotations {
		metricNameSlice := strings.Split(metricName, KubezSeparator)
		if len(metricNameSlice) < 2 {
			continue
		}

		switch metricNameSlice[len(metricNameSlice)-1] {
		case targetAverageUtilization:

		case targetAverageValue:

		}

	}

	switch kubezMetricType {
	case kubezCpuPrefix:
		// CPU metric
		metric := autoscalingv2.MetricSpec{
			Type: autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{
				Name: v1.ResourceCPU,
				Target: autoscalingv2.MetricTarget{
					Type:               autoscalingv2.UtilizationMetricType,
					AverageUtilization: utilpointer.Int32Ptr(annotations[TargetAverageUtilization]),
				},
			},
		}
		metrics = append(metrics, metric)
	case kubezMemoryPrefix:
		// TODO
	case kubezPrometheusPrefix:
		// TODO
	}

	if len(metrics) == 0 {
		return nil, fmt.Errorf("Could't parse metrics, the numbers is zero")
	}

	return metrics, nil
}

func IsOwnerReference(uid types.UID, ownerReferences []metav1.OwnerReference) bool {
	var isOwnerRef bool
	for _, ownerReferences := range ownerReferences {
		if uid == ownerReferences.UID {
			isOwnerRef = true
			break
		}
	}
	return isOwnerRef
}

func ManagerByKubezAutoscaler(hpa *autoscalingv2.HorizontalPodAutoscaler) bool {
	for _, managedField := range hpa.ManagedFields {
		if managedField.APIVersion == HorizontalPodAutoscalerAPIVersion &&
			managedField.Manager == "kubez-autoscaler-controller" {
			return true
		}
	}
	return false
}

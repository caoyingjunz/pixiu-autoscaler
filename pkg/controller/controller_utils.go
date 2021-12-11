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

package controller

import (
	"fmt"
	"strconv"
	"strings"

	autoscalingv2 "k8s.io/api/autoscaling/v2beta2"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientset "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	utilpointer "k8s.io/utils/pointer"
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

type Event string

const (
	Add    Event = "add"
	Update Event = "update"
	Delete Event = "delete"
)

type PixiuHpaSpec struct {
	Event Event
	Hpa   *autoscalingv2.HorizontalPodAutoscaler
}

func CreateHorizontalPodAutoscaler(
	name string,
	namespace string,
	uid types.UID,
	apiVersion string,
	kind string,
	annotations map[string]string) (*autoscalingv2.HorizontalPodAutoscaler, error) {

	minReplicas, err := extractReplicas(annotations, MinReplicas)
	if err != nil {
		return nil, fmt.Errorf("extract minReplicas from annotations failed: %v", err)
	}
	maxReplicas, err := extractReplicas(annotations, MaxReplicas)
	if err != nil {
		return nil, fmt.Errorf("extract maxReplicas from annotations failed: %v", err)
	}

	metrics, err := parseMetricSpecs(annotations)
	if err != nil {
		return nil, fmt.Errorf("parse metric specs from annotations failed: %v", err)
	}

	controller := true
	blockOwnerDeletion := true
	// Inject ownerReference label
	ownerReference := metav1.OwnerReference{
		APIVersion:         apiVersion,
		Kind:               kind,
		Name:               name,
		UID:                uid,
		Controller:         &controller,
		BlockOwnerDeletion: &blockOwnerDeletion,
	}

	spec := autoscalingv2.HorizontalPodAutoscalerSpec{
		MinReplicas: utilpointer.Int32Ptr(minReplicas),
		MaxReplicas: maxReplicas,
		ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
			APIVersion: apiVersion,
			Kind:       kind,
			Name:       name,
		},
		Metrics: metrics,
	}

	return &autoscalingv2.HorizontalPodAutoscaler{
		TypeMeta: metav1.TypeMeta{
			Kind:       HorizontalPodAutoscaler,
			APIVersion: AutoscalingAPIVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{
				ownerReference,
			},
		},
		Spec: spec,
	}, nil
}

// Parse and get metric type (valid is cpu and memory) and target
func getMetricTarget(metricName string) (string, string, error) {
	metricTypeSlice := strings.Split(metricName, PixiuDot)
	if len(metricTypeSlice) < 2 {
		return "", "", fmt.Errorf("invalied metric item %s", metricName)
	}
	metricType := metricTypeSlice[0]
	if metricType != cpu && metricType != memory {
		return "", "", fmt.Errorf("unsupprted metric resource name: %s", metricType)
	}

	metricTargetSlice := strings.Split(metricName, PixiuSeparator)
	if len(metricTargetSlice) < 2 {
		return "", "", fmt.Errorf("invalied metric item %s", metricName)
	}

	return metricType, metricTargetSlice[len(metricTargetSlice)-1], nil
}

func parseMetricSpecs(annotations map[string]string) ([]autoscalingv2.MetricSpec, error) {
	metricSpecs := make([]autoscalingv2.MetricSpec, 0)

	for metricName, metricValue := range annotations {
		// let it go if annotation item are not the target
		if !strings.Contains(metricName, PixiuDot+PixiuRootPrefix) {
			continue
		}
		metricType, target, err := getMetricTarget(metricName)
		if err != nil {
			return nil, err
		}

		var metricSpec autoscalingv2.MetricSpec
		switch target {
		case targetAverageUtilization:
			averageUtilization, err := extractAverageUtilization(metricValue)
			if err != nil {
				return nil, err
			}

			metricSpec = autoscalingv2.MetricSpec{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Target: autoscalingv2.MetricTarget{
						Type:               autoscalingv2.UtilizationMetricType,
						AverageUtilization: utilpointer.Int32Ptr(averageUtilization),
					},
				},
			}

		case targetAverageValue:
			averageValue, err := resource.ParseQuantity(metricValue)
			if err != nil {
				return nil, err
			}

			metricSpec = autoscalingv2.MetricSpec{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Target: autoscalingv2.MetricTarget{
						Type:         autoscalingv2.AverageValueMetricType,
						AverageValue: &averageValue,
					},
				},
			}
		}

		switch metricType {
		case cpu:
			metricSpec.Resource.Name = v1.ResourceCPU
		case memory:
			metricSpec.Resource.Name = v1.ResourceMemory
		}

		metricSpecs = append(metricSpecs, metricSpec)
	}

	if len(metricSpecs) == 0 {
		return nil, fmt.Errorf("could't parse metric specs, the numbers is zero")
	}

	return metricSpecs, nil
}

func IsOwnerReference(uid types.UID, ownerReferences []metav1.OwnerReference) bool {
	var isOwnerRef bool
	for _, ownerReference := range ownerReferences {
		if uid == ownerReference.UID {
			isOwnerRef = true
			break
		}
	}
	return isOwnerRef
}

func ManageByPixiuController(hpa *autoscalingv2.HorizontalPodAutoscaler) bool {
	for _, managedField := range hpa.ManagedFields {
		if managedField.APIVersion == AutoscalingAPIVersion &&
			(managedField.Manager == PixiuManager ||
				// This condition used for local run
				managedField.Manager == PixiuMain) {
			return true
		}
	}

	return false
}

func extractReplicas(annotations map[string]string, replicasType string) (int32, error) {
	var Replicas int64
	var err error
	switch replicasType {
	case MinReplicas:
		minReplicas, exists := annotations[MinReplicas]
		if !exists {
			// Default minReplicas is 1
			return 1, nil
		}
		if Replicas, err = strconv.ParseInt(minReplicas, 10, 32); err != nil {
			return 0, err
		}
	case MaxReplicas:
		maxReplicas, exists := annotations[MaxReplicas]
		if !exists {
			return 0, fmt.Errorf("%s is required", MaxReplicas)
		}
		Replicas, err = strconv.ParseInt(maxReplicas, 10, 32)
	}

	return int32(Replicas), err
}

func extractAverageUtilization(averageUtilization string) (int32, error) {
	value64, err := strconv.ParseInt(averageUtilization, 10, 32)
	if err != nil {
		return 0, err
	}
	if value64 <= 0 && value64 > 100 {
		return 0, fmt.Errorf("averageUtilization should be range 1 between 100")
	}

	return int32(value64), nil
}

// Empty is public since it is used by some internal API objects for conversions between external
// string arrays and internal sets, and conversion logic requires public types today.
type Empty struct{}

func NewItems() map[string]Empty {
	items := make(map[string]Empty)
	for _, k := range []string{cpuAverageUtilization, memoryAverageUtilization, cpuAverageValue, memoryAverageValue} {
		items[k] = Empty{}
	}

	return items
}

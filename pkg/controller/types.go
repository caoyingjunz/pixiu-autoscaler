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

import "fmt"

const (
	KubezRootPrefix                 string = "hpa.caoyingjunz.autoscaler"
	KubezAnnotationSeparator        string = "/"
	KubezMetricName                 string = "metricType"
	kubezCpuAnnotationPrefix        string = "cpu"
	kubezMemoryAnnotationPrefix     string = "memory"
	kubezPrometheusAnnotationPrefix string = "prometheus"

	MinReplicas              string = "minReplicas"
	MaxReplicas              string = "maxReplicas"
	TargetAverageUtilization string = "targetAverageUtilization"
)

func PrecheckAndFilterAnnotations(annotations map[string]string) (map[string]string, error) {
	kubezAnnotations := make(map[string]string)
	metricType, exists := annotations[KubezRootPrefix+KubezAnnotationSeparator+KubezMetricName]
	if !exists {
		return nil, fmt.Errorf("hpa.caoyingjunz.autoscaler/metricType required")
	}
	kubezAnnotations[KubezRootPrefix+KubezAnnotationSeparator+KubezMetricName] = metricType

	switch metricType {
	case kubezCpuAnnotationPrefix:
		// TODO: 需要检查 minReplicas 和 maxReplicas 的类型
		minReplicas, exists := annotations[KubezRootPrefix+KubezAnnotationSeparator+MinReplicas]
		if !exists {
			minReplicas = "1"
		}
		kubezAnnotations[KubezRootPrefix+KubezAnnotationSeparator+MinReplicas] = minReplicas

		maxReplicas, exists := annotations[KubezRootPrefix+KubezAnnotationSeparator+MaxReplicas]
		if !exists {
			return nil, fmt.Errorf("hpa.caoyingjunz.autoscaler/MaxReplicas required")
		}
		kubezAnnotations[KubezRootPrefix+KubezAnnotationSeparator+MaxReplicas] = maxReplicas

		targetCPUUtilizationPercentage, exists := annotations[KubezRootPrefix+KubezAnnotationSeparator+TargetAverageUtilization]
		if !exists {
			targetCPUUtilizationPercentage = "80"
		}
		kubezAnnotations[KubezRootPrefix+KubezAnnotationSeparator+TargetAverageUtilization] = targetCPUUtilizationPercentage
	default:
		return nil, fmt.Errorf("hpa.caoyingjunz.autoscaler/metricType is valided")
	}

	return kubezAnnotations, nil
}

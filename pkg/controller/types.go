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

const (
	KubezRootPrefix string = "hpa.caoyingjunz.io"
	KubezSeparator  string = "/"

	kubezCpuPrefix        string = "cpu"
	kubezMemoryPrefix     string = "memory"
	kubezPrometheusPrefix string = "prometheus"

	MinReplicas        string = "minReplicas"
	MaxReplicas        string = "maxReplicas"
	AverageUtilization string = "AverageUtilization"
)

func PrecheckAndFilterAnnotations(annotations map[string]string) (map[string]string, error) {
	kubezAnnotations := make(map[string]string)

	averageUtilization := annotations["cpu."+KubezRootPrefix+KubezSeparator+AverageUtilization]

	kubezAnnotations["cpu."+KubezRootPrefix+KubezSeparator+AverageUtilization] = averageUtilization
	// TODO: 需要检查 minReplicas 和 maxReplicas 的类型
	minReplicas, exists := annotations["hpa.caoyingjunz.io/minReplicas"]
	if !exists {
		minReplicas = "1"
	}
	kubezAnnotations["hpa.caoyingjunz.io/minReplicas"] = minReplicas

	maxReplicas, exists := annotations["hpa.caoyingjunz.io/maxReplicas"]
	if !exists {
		maxReplicas = "6"
	}
	kubezAnnotations["hpa.caoyingjunz.io/maxReplicas"] = maxReplicas

	return kubezAnnotations, nil
}

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
)

const (
	KubezRootPrefix string = "hpa.caoyingjunz.io"
	KubezSeparator  string = "/"

	kubezCpuPrefix        string = "cpu"
	kubezMemoryPrefix     string = "memory"
	kubezPrometheusPrefix string = "prometheus"

	MinReplicas        string = "hpa.caoyingjunz.io/minReplicas"
	MaxReplicas        string = "hpa.caoyingjunz.io/maxReplicas"
	AverageUtilization string = "hpa.caoyingjunz.io/AverageUtilization"

	cpuAverageUtilization    = kubezCpuPrefix + "." + AverageUtilization
	memoryAverageUtilization = kubezMemoryPrefix + "." + AverageUtilization
)

func PrecheckAndFilterAnnotations(annotations map[string]string) (map[string]int32, error) {
	hpaAnnotations := make(map[string]int32)

	averageUtilization, exists := annotations[cpuAverageUtilization]
	if !exists {
		return nil, fmt.Errorf("%s is required", cpuAverageUtilization)
	}
	averageUtilizationInt64, err := strconv.ParseInt(averageUtilization, 10, 32)
	if err != nil {
		return nil, err
	}
	if averageUtilizationInt64 <= 0 || averageUtilizationInt64 > 100 {
		return nil, fmt.Errorf("averageUtilization should be range 1, 100")
	}
	hpaAnnotations[cpuAverageUtilization] = int32(averageUtilizationInt64)

	maxReplicas, exists := annotations[MaxReplicas]
	if !exists {
		return nil, fmt.Errorf("%s is required", MaxReplicas)
	}
	maxReplicasInt64, err := strconv.ParseInt(maxReplicas, 10, 32)
	if err != nil {
		return nil, err
	}
	hpaAnnotations[MaxReplicas] = int32(maxReplicasInt64)

	var minReplicasInt64 int64
	minReplicas, exists := annotations[MinReplicas]
	if exists {
		minReplicasInt64, err = strconv.ParseInt(minReplicas, 10, 32)
		if err != nil {
			return nil, err
		}
	} else {
		// Default minReplicas is 1
		minReplicasInt64 = int64(1)
	}
	hpaAnnotations[MinReplicas] = int32(minReplicasInt64)

	return hpaAnnotations, nil
}

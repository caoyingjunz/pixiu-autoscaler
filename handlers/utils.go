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
	"fmt"
	"strconv"
)

func parseKubezAutoscaler(annotations map[string]string) (int32, int32, error) {
	// 获取 hpa 所需要的参数
	minRcs, minOk := annotations[minReplicas]
	maxRcs, maxOk := annotations[maxReplicas]
	//targetCPU, cpuExist := deployment.Annotations[targetCPUUtilizationPercentage]

	var minInt32, maxInt32 int32
	if minOk {
		minRcsInt, err := strconv.ParseInt(minRcs, 10, 32)
		if err != nil {
			return minInt32, maxInt32, err
		}
		minInt32 = int32(minRcsInt)
	}
	if maxOk {
		maxRcsInt, err := strconv.ParseInt(maxRcs, 10, 32)
		if err != nil {
			return minInt32, maxInt32, err
		}
		maxInt32 = int32(maxRcsInt)
	}

	if minInt32 == 0 {
		minInt32 = 1
	}

	if maxInt32 == 0 {
		return minInt32, maxInt32, fmt.Errorf("maxReplicas is requred")
	}
	return minInt32, maxInt32, nil
}

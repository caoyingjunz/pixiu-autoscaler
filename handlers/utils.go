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

func needToBeRecover(annotations map[string]string) bool {
	_, ok := annotations[maxReplicas]
	return ok
}

func parseKubezAutoscaler(annotations map[string]string) (map[string]int32, error) {
	// precheck and parse kubez annotations
	// return the map contains the required parameter from annotations with map
	var minInt32, maxInt32, targetCPUUtilizationPercentageInt32 int32

	// maxReplicas
	maxRcs, maxOk := annotations[maxReplicas]
	if maxOk {
		maxRcsInt, err := strconv.ParseInt(maxRcs, 10, 32)
		if err != nil {
			return nil, err
		}
		maxInt32 = int32(maxRcsInt)
	}
	if maxInt32 == 0 {
		return nil, fmt.Errorf("maxReplicas is requred")
	}

	// minReplicas
	minRcs, minOk := annotations[minReplicas]
	if minOk {
		minRcsInt, err := strconv.ParseInt(minRcs, 10, 32)
		if err != nil {
			return nil, err
		}
		minInt32 = int32(minRcsInt)
	}
	if minInt32 == 0 {
		minInt32 = 1
	}

	// targetCPUUtilizationPercentage
	targetPercentage, targetOk := annotations[targetCPUUtilizationPercentage]
	if targetOk {
		targetInt, err := strconv.ParseInt(targetPercentage, 10, 32)
		if err != nil {
			return nil, err
		}
		targetCPUUtilizationPercentageInt32 = int32(targetInt)
	}
	if targetCPUUtilizationPercentageInt32 > 100 || targetCPUUtilizationPercentageInt32 < 0 {
		return nil, fmt.Errorf("targetCPUUtilizationPercentage range must be 0 through 100")
	}
	if targetCPUUtilizationPercentageInt32 == 0 {
		targetCPUUtilizationPercentageInt32 = 80
	}

	return map[string]int32{
		minReplicas:                    minInt32,
		maxReplicas:                    maxInt32,
		targetCPUUtilizationPercentage: targetCPUUtilizationPercentageInt32,
	}, nil
}

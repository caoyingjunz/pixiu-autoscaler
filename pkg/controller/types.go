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

const (
	PixiuManager    string = "kubez-autoscaler-controller"
	PixiuMain       string = "main" // Just for local test
	PixiuRootPrefix string = "hpa.caoyingjunz.io"
	PixiuSeparator  string = "/"
	PixiuDot        string = "."

	MinReplicas              string = "hpa.caoyingjunz.io/minReplicas"
	MaxReplicas              string = "hpa.caoyingjunz.io/maxReplicas"
	targetAverageUtilization string = "targetAverageUtilization"
	targetAverageValue       string = "targetAverageValue"

	// TODO: prometheus is not supported for now.
	cpu        string = "cpu"
	memory     string = "memory"
	prometheus string = "prometheus"

	cpuAverageUtilization        = "cpu." + PixiuRootPrefix + PixiuSeparator + targetAverageUtilization
	memoryAverageUtilization     = "memory." + PixiuRootPrefix + PixiuSeparator + targetAverageUtilization
	prometheusAverageUtilization = "prometheus." + PixiuRootPrefix + PixiuSeparator + targetAverageUtilization

	// CPU, in cores. (500m = .5 cores)
	// Memory, in bytes. (500Gi = 500GiB = 500 * 1024 * 1024 * 1024)
	cpuAverageValue        = "cpu." + PixiuRootPrefix + PixiuSeparator + targetAverageValue
	memoryAverageValue     = "memory." + PixiuRootPrefix + PixiuSeparator + targetAverageValue
	prometheusAverageValue = "prometheus." + PixiuRootPrefix + PixiuSeparator + targetAverageValue
)

const (
	AppsAPIVersion        string = "apps/v1"
	AutoscalingAPIVersion string = "autoscaling/v2beta2"

	Deployment              string = "Deployment"
	StatefulSet             string = "StatefulSet"
	HorizontalPodAutoscaler string = "HorizontalPodAutoscaler"
)

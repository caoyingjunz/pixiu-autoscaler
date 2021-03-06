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

package kubezresources

import (
	apps "k8s.io/api/apps/v1"
	"k8s.io/klog"
)

// Responsible for depolyments
func AddDeployment(obj interface{}) {
	d := obj.(*apps.Deployment)
	klog.V(0).Infof("Adding Deployment %s/%s", d.Namespace, d.Name)
}

func UpdateDeployment(old, current interface{}) {
	oldD := old.(*apps.Deployment)
	curD := current.(*apps.Deployment)
	klog.V(0).Infof("Updating Deployment %s/%s", oldD.Namespace, curD.Name)
}

func DeleteDeployment(obj interface{}) {
	d := obj.(apps.Deployment)
	klog.V(0).Infof("Deleting Deployment %s/%s", d.Namespace, d.Name)
}

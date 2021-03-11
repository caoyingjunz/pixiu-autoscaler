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

package options

import (
	cliflag "k8s.io/component-base/cli/flag"

	"github.com/caoyingjunz/kubez-autoscaler/cmd/app/config"
)

// Options has all the params needed to run a Autoscaler
// TODO: for new, the params is just LeaderElection
type Options struct {
	ComponentConfig config.KubezConfiguration
}

func NewKubezOptions() (*Options, error) {

	cfg := config.KubezConfiguration{}
	o := &Options{
		ComponentConfig: cfg,
	}

	return o, nil
}

// Flags returns flags for a specific scheduler by section name
func (o *Options) Flags() (nfs cliflag.NamedFlagSets) {
	fs := nfs.FlagSets("misc")

	config.BindFlags(&o.ComponentConfig.LeaderElection.LeaderElectionConfiguration, nfs.FlagSet("leader election"))

	return nfs
}

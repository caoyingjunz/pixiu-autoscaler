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
	"github.com/spf13/cobra"
	v1 "k8s.io/api/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	clientgokubescheme "k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog"

	"github.com/caoyingjunz/kubez-autoscaler/cmd/app/config"
	"github.com/caoyingjunz/kubez-autoscaler/pkg/controller"
)

const (
	// KubezControllerManagerUserAgent is the userAgent name when starting kubez-autoscaler managers.
	KubezControllerManagerUserAgent = "kubez-autoscaler-manager"
)

// Options has all the params needed to run a Autoscaler
// TODO: for new, the params is just LeaderElection
type Options struct {
	ComponentConfig config.KubezConfiguration

	// ConfigFile is the location of the autoscaler's configuration file.
	ConfigFile string

	Master string
}

func NewOptions() (*Options, error) {

	cfg := config.KubezConfiguration{}
	o := &Options{
		ComponentConfig: cfg,
	}

	return o, nil
}

var (
	leaderElect bool
)

// BindFlags binds the KubezConfiguration struct fields to a Options
func (o *Options) BindFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVarP(&leaderElect, "leader-elect", "l", false, ""+
		"Start a leader election client and gain leadership before "+
		"executing the main loop. Enable this when running replicated "+
		"components for high availability.")

}

func createRecorder(kubeClient clientset.Interface, userAgent string) record.EventRecorder {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	return eventBroadcaster.NewRecorder(clientgokubescheme.Scheme, v1.EventSource{Component: userAgent})
}

// Config return a kubez controller manager config objective
func (o *Options) Config() (*config.KubezConfiguration, error) {
	kubeConfig, err := config.BuildKubeConfig()
	if err != nil {
		return nil, err
	}
	kubeConfig.QPS = 30000
	kubeConfig.Burst = 30000

	clientBuilder := controller.SimpleControllerClientBuilder{
		ClientConfig: kubeConfig,
	}

	client := clientBuilder.ClientOrDie("leader-client")
	eventRecorder := createRecorder(client, KubezControllerManagerUserAgent)

	c := &config.KubezConfiguration{
		LeaderClient:  client,
		EventRecorder: eventRecorder,
	}
	// TODO
	c.LeaderElection.LeaderElect = leaderElect

	return c, nil
}

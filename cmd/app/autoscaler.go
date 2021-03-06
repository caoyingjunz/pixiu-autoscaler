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

package app

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"k8s.io/klog"

	"github.com/caoyingjunz/kubez-autoscaler/pkg/controller"
	"github.com/caoyingjunz/kubez-autoscaler/pkg/controller/autoscaler"
)

const (
	workers = 5
)

// NewAutoscalerCommand creates a *cobra.Command object with default parameters
func NewAutoscalerCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use: "kubez-autoscaler",
		Long: `The kubez autoscaler manager is a daemon than embeds
the core control loops shipped with advanced HPA.`,
		Run: func(cmd *cobra.Command, args []string) {
			if err := Run(); err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				os.Exit(1)
			}
		},
		Args: func(cmd *cobra.Command, args []string) error {
			for _, arg := range args {
				if len(arg) > 0 {
					return fmt.Errorf("%q does not take any arguments, got %q", cmd.CommandPath(), args)
				}
			}
			return nil
		},
	}
	return cmd
}

func Run() error {
	// TODO(caoyingjunz): leader-elect, healthzCheck, and readyzCheck will be added later.
	stopCh := make(chan struct{})
	defer close(stopCh)

	var config *rest.Config
	config, err := rest.InClusterConfig()
	if err != nil {
		klog.Warning("Geting config from In-Cluster failed, Try fetching config from HomeDir")
		config, err = clientcmd.BuildConfigFromFlags("", filepath.Join(homedir.HomeDir(), ".kube", "config"))
		if err != nil {
			return err
		}
	}

	rootClientBuilder := controller.SimpleControllerClientBuilder{
		ClientConfig: config,
	}

	versionedClient := rootClientBuilder.ClientOrDie("shared-informers")
	InformerFactory := informers.NewSharedInformerFactory(versionedClient, time.Minute)

	ac, err := autoscaler.NewAutoscalerController(
		InformerFactory.Apps().V1().Deployments(),
		InformerFactory.Autoscaling().V1().HorizontalPodAutoscalers(),
		rootClientBuilder.ClientOrDie("shared-informers"),
	)
	if err != nil {
		return err
	}
	go ac.Run(workers, stopCh)

	select {}

	return nil
}

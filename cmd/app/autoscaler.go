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

// Package app implements a Server object for running the autoscaler.
package app

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog"

	"github.com/caoyingjunz/kubez-autoscaler/cmd/app/config"
	"github.com/caoyingjunz/kubez-autoscaler/cmd/app/options"
	"github.com/caoyingjunz/kubez-autoscaler/pkg/controller"
	"github.com/caoyingjunz/kubez-autoscaler/pkg/controller/autoscaler"
)

const (
	workers = 5
)

// NewAutoscalerCommand creates a *cobra.Command object with default parameters
func NewAutoscalerCommand() *cobra.Command {
	s, err := options.NewOptions()
	if err != nil {
		klog.Fatalf("unable to initialize command options: %v", err)
	}

	cmd := &cobra.Command{
		Use: "kubez-autoscaler",
		Long: `The kubez autoscaler manager is a daemon than embeds
the core control loops shipped with advanced HPA.`,
		Run: func(cmd *cobra.Command, args []string) {
			c, err := s.Config()
			if err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				os.Exit(1)
			}
			if err := Run(c); err != nil {
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

// Run runs the kubez-autoscaler process. This should never exit.
func Run(c *config.KubezConfiguration) error {
	stopCh := make(chan struct{})
	defer close(stopCh)

	kubeConfig, err := config.BuildKubeConfig()
	if err != nil {
		return err
	}

	run := func(ctx context.Context) {
		clientBuilder := controller.SimpleControllerClientBuilder{
			ClientConfig: kubeConfig,
		}

		kubezCtx, err := CreateControllerContext(clientBuilder, clientBuilder, ctx.Done())
		if err != nil {
			klog.Fatal("create kubez context failed: %v", err)
		}

		ac, err := autoscaler.NewAutoscalerController(
			kubezCtx.InformerFactory.Apps().V1().Deployments(),
			kubezCtx.InformerFactory.Apps().V1().StatefulSets(),
			kubezCtx.InformerFactory.Autoscaling().V1().HorizontalPodAutoscalers(),
			clientBuilder.ClientOrDie("shared-informers"),
		)
		if err != nil {
			klog.Fatalf("error new autoscaler controller: %v", err)
		}
		go ac.Run(workers, stopCh)

		kubezCtx.InformerFactory.Start(stopCh)
		kubezCtx.ObjectOrMetadataInformerFactory.Start(stopCh)
		select {}
	}

	// TODO
	leaderElect := true
	if !leaderElect {
		run(context.TODO())
		panic("unreachable")
	}

	id, err := os.Hostname()
	if err != nil {
		return err
	}

	// add a uniquifier so that two processes on the same host don't accidentally both become active
	id = id + "_" + string(uuid.NewUUID())

	leaderClientBuilder := controller.SimpleControllerClientBuilder{
		ClientConfig: kubeConfig,
	}
	leaderClient := leaderClientBuilder.ClientOrDie("shared-informers")
	rl, err := resourcelock.New("endpointsleases",
		"kube-system",
		"kubez-autoscaler-manager",
		leaderClient.CoreV1(),
		leaderClient.CoordinationV1(),
		resourcelock.ResourceLockConfig{
			Identity: id,
			//EventRecorder: c.EventRecorder,
		})
	if err != nil {
		klog.Fatalf("error creating lock: %v", err)
	}

	leaderelection.RunOrDie(context.TODO(), leaderelection.LeaderElectionConfig{
		Lock:          rl,
		LeaseDuration: time.Second * 15,
		RenewDeadline: time.Second * 10,
		RetryPeriod:   time.Second * 2,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: run,
			OnStoppedLeading: func() {
				klog.Fatalf("leaderelection lost")
			},
		},
		//WatchDog: electionChecker,
		Name: "kubez-autoscaler-manager",
	})
	panic("unreachable")

	return nil
}

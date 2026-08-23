// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/operator/internal/controller"
)

// version is overridable at build time via -ldflags "-X main.version=<tag>".
var version = "dev"

func main() {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	ctrl.Log.Info("starting snapshot operator", "version", version)

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		ctrl.Log.Error(err, "unable to register client-go scheme")
		os.Exit(1)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		ctrl.Log.Error(err, "unable to register API types")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                        scheme,
		HealthProbeBindAddress:        ":8081",
		LeaderElection:                true,
		LeaderElectionID:              "snapshot-operator.nvidia.com",
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		ctrl.Log.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	podSnapshotReconciler := &controller.PodSnapshotReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Recorder:  mgr.GetEventRecorderFor("podsnapshot-controller"),
	}
	if err := podSnapshotReconciler.SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to set up PodSnapshot controller")
		os.Exit(1)
	}

	snapshotJobReconciler := &controller.SnapshotJobReconciler{
		Client:   mgr.GetClient(),
		Recorder: mgr.GetEventRecorderFor("snapshotjob-controller"),
	}
	if err := snapshotJobReconciler.SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to set up SnapshotJob controller")
		os.Exit(1)
	}

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.Error(err, "unable to start manager")
		os.Exit(1)
	}
}

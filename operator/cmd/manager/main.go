// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/operator/internal/controller"
	"github.com/ai-dynamo/snapshot/operator/internal/maintenance"
	operatortypes "github.com/ai-dynamo/snapshot/operator/internal/types"
)

// version is overridable at build time via -ldflags "-X main.version=<tag>".
var version = "dev"

func main() {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	storageConfigPath := flag.String("snapshot-storage-config", "", "Path to resolved storage configuration shared with PageBroker")
	artifactCleanupConfig := bindArtifactCleanupFlags(flag.CommandLine)
	artifactCleanupS3CredentialsPath, artifactCleanupS3CABundlePath := bindArtifactCleanupS3Flags(flag.CommandLine)
	cuInterposeConfig := bindCuInterposeFlags(flag.CommandLine)
	testFailureAnnotation, err := bindTestHookFlags(flag.CommandLine)
	if err != nil {
		ctrl.Log.Error(err, "invalid E2E test hook configuration")
		os.Exit(1)
	}
	bindNewContents := flag.Bool("bind-new-contents", false,
		"Bind new content to the configured store after all agents support artifact addressing")
	pageBrokerS3ConfigNamespace := flag.String("pagebroker-s3-config-namespace", "",
		"Namespace of the ConfigMap PageBroker reads its S3 configuration from; defaults to POD_NAMESPACE")
	pageBrokerS3ConfigName := flag.String("pagebroker-s3-config-name", DefaultPageBrokerS3ConfigName,
		"Name of the ConfigMap PageBroker reads its S3 configuration from")
	releaseName := flag.String("release-name", "",
		"Helm release name, used to label the PageBroker S3 configuration ConfigMap this binary writes outside Helm's own apply step")
	flag.Parse()
	storageCfg, storeID := mustStorageConfig(*storageConfigPath)
	captureStoreID, err := newContentStoreID(storeID, *bindNewContents)
	if err != nil {
		ctrl.Log.Error(err, "invalid content binding configuration")
		os.Exit(1)
	}
	if *pageBrokerS3ConfigNamespace == "" {
		*pageBrokerS3ConfigNamespace = os.Getenv("POD_NAMESPACE")
	}
	if storageCfg.S3 != nil {
		if *pageBrokerS3ConfigNamespace == "" {
			ctrl.Log.Error(fmt.Errorf("missing namespace"),
				"pagebroker-s3-config-namespace is required with an S3 storage configuration "+
					"when POD_NAMESPACE is not set")
			os.Exit(1)
		}
		artifactCleanupConfig.S3 = &operatortypes.S3Config{
			Bucket:          storageCfg.S3.Bucket,
			Prefix:          storageCfg.S3.Prefix,
			Region:          storageCfg.S3.Region,
			Endpoint:        storageCfg.S3.Endpoint,
			CredentialsPath: *artifactCleanupS3CredentialsPath,
			CABundlePath:    *artifactCleanupS3CABundlePath,
		}
	}
	artifactCleanupConfig.StoreID = storeID
	if err := artifactCleanupConfig.Validate(); err != nil {
		ctrl.Log.Error(err, "invalid artifact cleanup configuration")
		os.Exit(1)
	}
	if err := cuInterposeConfig.Validate(); err != nil {
		ctrl.Log.Error(err, "invalid cuinterpose configuration")
		os.Exit(1)
	}

	ctrl.Log.Info("starting snapshot operator", "version", version)
	if storeID != "" {
		ctrl.Log.Info("configured storage store", "storeID", storeID)
	}

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

	if storageCfg.S3 != nil {
		// A direct (non-cached) client: this runs once at startup, before the
		// manager's cache is started, and writes rather than watches.
		directClient, err := client.New(mgr.GetConfig(), client.Options{Scheme: scheme})
		if err != nil {
			ctrl.Log.Error(err, "unable to construct client for PageBroker S3 configuration")
			os.Exit(1)
		}
		labels := map[string]string{"app.kubernetes.io/part-of": "snapshot", "app.kubernetes.io/managed-by": "snapshot-operator"}
		if *releaseName != "" {
			labels["app.kubernetes.io/instance"] = *releaseName
		}
		if err := ensurePageBrokerS3Config(context.Background(), directClient,
			*pageBrokerS3ConfigNamespace, *pageBrokerS3ConfigName, labels, *storageCfg.S3, storeID); err != nil {
			ctrl.Log.Error(err, "unable to write PageBroker S3 configuration")
			os.Exit(1)
		}
	}

	podSnapshotReconciler := &controller.PodSnapshotReconciler{
		Client:                mgr.GetClient(),
		NonCacheReadClient:    mgr.GetAPIReader(),
		Recorder:              mgr.GetEventRecorderFor("podsnapshot-controller"),
		TestFailureAnnotation: *testFailureAnnotation,
		StoreID:               captureStoreID,
	}
	if err := podSnapshotReconciler.SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to set up PodSnapshot controller")
		os.Exit(1)
	}

	maintenanceQueue, err := maintenance.NewQueue(
		context.Background(),
		mgr.GetClient(),
		mgr.GetAPIReader(),
		mgr.GetEventRecorderFor("podsnapshotcontent-artifact-cleanup"),
		*artifactCleanupConfig,
	)
	if err != nil {
		ctrl.Log.Error(err, "unable to construct maintenance workqueue")
		os.Exit(1)
	}
	if err := mgr.Add(maintenanceQueue); err != nil {
		ctrl.Log.Error(err, "unable to set up maintenance workqueue")
		os.Exit(1)
	}

	if err := controller.SetupSnapshotContentReconciler(mgr, maintenanceQueue); err != nil {
		ctrl.Log.Error(err, "unable to set up PodSnapshotContent artifact cleanup controller")
		os.Exit(1)
	}

	snapshotJobReconciler := &controller.SnapshotJobReconciler{
		Client:               mgr.GetClient(),
		NonCacheReadClient:   mgr.GetAPIReader(),
		Recorder:             mgr.GetEventRecorderFor("snapshotjob-controller"),
		CuInterposeContainer: *cuInterposeConfig,
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

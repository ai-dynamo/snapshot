// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// crd-installer applies the CRDs embedded in the api module, as the operator
// Deployment's init container. Failing here deliberately keeps the manager from
// starting: an operator running against outdated CRDs is worse than one that
// has not started.
package main

import (
	"os"

	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ai-dynamo/snapshot/api/v1alpha1/crds"
	"github.com/ai-dynamo/snapshot/operator/internal/crdinstaller"
	"github.com/ai-dynamo/snapshot/operator/internal/logging"
)

func main() {
	log := logging.ConfigureLogger("stdout").WithName("crd-installer")
	ctrl.SetLogger(log)
	log.Info("Starting snapshot CRD installer")

	cfg, err := ctrl.GetConfig()
	if err != nil {
		log.Error(err, "unable to load kubeconfig")
		os.Exit(1)
	}
	cl, err := client.New(cfg, client.Options{})
	if err != nil {
		log.Error(err, "unable to create API client")
		os.Exit(1)
	}
	cs, err := apiextensionsclientset.NewForConfig(cfg)
	if err != nil {
		log.Error(err, "unable to create apiextensions client")
		os.Exit(1)
	}

	results, err := crdinstaller.InstallCRDs(ctrl.SetupSignalHandler(), cl,
		cs.ApiextensionsV1().CustomResourceDefinitions(), log, crds.All())
	if err != nil {
		log.Error(err, "CRD installation failed")
		os.Exit(1)
	}

	if results.Changed() {
		log.Info("CRDs installed", "count", len(results))
		return
	}
	log.Info("CRDs already up to date, no changes applied", "count", len(results))
}

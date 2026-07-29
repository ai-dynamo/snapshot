// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// crd-installer applies the CustomResourceDefinition manifests baked into the
// operator image with server-side apply. It runs as the operator Deployment's
// init container so a `helm upgrade` — which by design leaves the chart's crds/
// directory alone — still lands the definitions the manager expects before it
// starts. When the cluster already has them the run is a no-op.
package main

import (
	"flag"
	"os"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ai-dynamo/snapshot/operator/internal/crdinstaller"
	"github.com/ai-dynamo/snapshot/operator/internal/logging"
)

// version is overridable at build time via -ldflags "-X main.version=<tag>".
var version = "dev"

func main() {
	crdDir := flag.String("crd-dir", "/etc/snapshot/crds",
		"Directory holding the CRD manifests to apply. Defaults to the copy baked into the operator image.")
	flag.Parse()

	log := logging.ConfigureLogger("stdout").WithName("crd-installer")
	ctrl.SetLogger(log)
	log.Info("Starting snapshot CRD installer", "version", version, "crdDir", *crdDir)

	if err := run(*crdDir, log); err != nil {
		log.Error(err, "CRD installation failed")
		os.Exit(1)
	}
}

func run(crdDir string, log logr.Logger) error {
	docs, err := crdinstaller.LoadDir(crdDir)
	if err != nil {
		return err
	}
	if len(docs) == 0 {
		log.Info("No CRD manifests found, nothing to do", "crdDir", crdDir)
		return nil
	}

	cfg, err := ctrl.GetConfig()
	if err != nil {
		return err
	}
	cl, err := client.New(cfg, client.Options{})
	if err != nil {
		return err
	}

	ctx := ctrl.SetupSignalHandler()
	results, err := crdinstaller.InstallCRDs(ctx, cl, log, docs)
	if err != nil {
		return err
	}

	if !crdinstaller.Changed(results) {
		log.Info("CRDs already up to date, no changes applied", "count", len(results))
		return nil
	}
	log.Info("CRDs installed", "count", len(results))
	return nil
}

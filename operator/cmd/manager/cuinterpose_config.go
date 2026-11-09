// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"

	operatortypes "github.com/ai-dynamo/snapshot/operator/internal/types"
)

// cuInterposeWebhookConfiguration says whether and where the manager serves the
// Pod mutating webhook that prepares opted-in Pods SnapshotJob does not create.
type cuInterposeWebhookConfiguration struct {
	Enabled bool
	Port    int
	CertDir string
}

// Validate checks the webhook settings against the preparation mode. The webhook
// is the no-NRI path for Pods SnapshotJob does not create, so it runs only where
// SnapshotJob sources get init containers too, and uses the same installer image.
func (c cuInterposeWebhookConfiguration) Validate(preparation operatortypes.CuInterposeContainerConfiguration) error {
	if !c.Enabled {
		return nil
	}
	if !preparation.AddsInitContainer() {
		return fmt.Errorf("cuinterpose-webhook needs --multi-gpu-preparation=%s, got %q",
			operatortypes.MultiGPUPreparationInitContainer, preparation.Preparation)
	}
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("cuinterpose-webhook-port must be between 1 and 65535, got %d", c.Port)
	}
	if c.CertDir == "" {
		return fmt.Errorf("cuinterpose-webhook-cert-dir is required when the webhook is enabled")
	}
	return nil
}

func bindCuInterposeWebhookFlags(flags *flag.FlagSet) *cuInterposeWebhookConfiguration {
	cfg := &cuInterposeWebhookConfiguration{}
	flags.BoolVar(&cfg.Enabled, "cuinterpose-webhook", false,
		"Serve the mutating admission webhook that prepares Pods annotated nvidia.com/snapshot-multi-gpu: enabled for cuinterpose; "+
			"needs --multi-gpu-preparation=initContainer, whose installer image it uses")
	flags.IntVar(&cfg.Port, "cuinterpose-webhook-port", 9443, "Port the mutating admission webhook listens on")
	flags.StringVar(&cfg.CertDir, "cuinterpose-webhook-cert-dir", "",
		"Directory holding the webhook's tls.crt and tls.key, re-read when they change")
	return cfg
}

func bindCuInterposeFlags(flags *flag.FlagSet) *operatortypes.CuInterposeContainerConfiguration {
	cfg := &operatortypes.CuInterposeContainerConfiguration{Preparation: operatortypes.MultiGPUPreparationAgent}
	flags.StringVar((*string)(&cfg.Preparation), "multi-gpu-preparation", string(operatortypes.MultiGPUPreparationAgent),
		"Who prepares opted-in SnapshotJob sources for multi-GPU checkpoints: agent (the node agent, needs NRI) or initContainer (an init container the operator adds)")
	flags.StringVar(&cfg.Image, "cuinterpose-image", "",
		"Image that installs the cuinterpose libraries and launcher into opted-in SnapshotJob sources; required when --multi-gpu-preparation=initContainer")
	flags.StringVar((*string)(&cfg.PullPolicy), "cuinterpose-image-pull-policy", "",
		"Pull policy for the cuinterpose install init container (Always, IfNotPresent, Never); empty uses the Kubernetes default")
	return cfg
}

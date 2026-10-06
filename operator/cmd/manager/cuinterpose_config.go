// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"

	operatortypes "github.com/ai-dynamo/snapshot/operator/internal/types"
)

func bindCuInterposeFlags(flags *flag.FlagSet) *operatortypes.CuInterposeContainerConfiguration {
	cfg := &operatortypes.CuInterposeContainerConfiguration{}
	flags.StringVar(&cfg.Image, "cuinterpose-image", "",
		"Image that installs the cuinterpose libraries and launcher into opted-in SnapshotJob sources")
	flags.StringVar((*string)(&cfg.PullPolicy), "cuinterpose-image-pull-policy", "",
		"Pull policy for the cuinterpose install init container (Always, IfNotPresent, Never); empty uses the Kubernetes default")
	return cfg
}

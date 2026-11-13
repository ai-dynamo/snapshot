// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"

	operatortypes "github.com/ai-dynamo/snapshot/operator/internal/types"
)

func bindCheckpointFlags(flags *flag.FlagSet) *operatortypes.CheckpointConfig {
	cfg := &operatortypes.CheckpointConfig{}
	flags.DurationVar(&cfg.CompletionGrace, "checkpoint-completion-grace", operatortypes.DefaultCheckpointCompletionGrace,
		"How long a checkpoint may wait for the node agent's result, counted from when its source pod becomes terminal (not the overall checkpoint timeout); must cover the largest snapshot's upload")
	return cfg
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

func validatePreflight(cfg *types.AgentConfig) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validate configuration: %w", err)
	}
	if err := nsmount.ValidateBasePathMount(cfg.Storage.BasePath); err != nil {
		return fmt.Errorf("validate checkpoint storage mount: %w", err)
	}
	if err := executor.ValidateCheckpointStorage(cfg.Storage.BasePath); err != nil {
		return fmt.Errorf("validate checkpoint storage publication: %w", err)
	}
	return nil
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"time"
)

// DefaultCheckpointCompletionGrace keeps the 5-minute window the operator used
// before the grace became configurable.
const DefaultCheckpointCompletionGrace = 5 * time.Minute

// CheckpointConfig holds the operator's checkpoint settings. The node agent's
// own checkpoint settings, such as the timeout and retry limit, live in the
// agent's CheckpointSpec and are not read here.
type CheckpointConfig struct {
	// CompletionGrace is how long a pending checkpoint may wait for the node
	// agent's result, counted from when its source pod turned terminal, before
	// the PodSnapshot fails with CheckpointResultTimeout. It is the backstop
	// for an agent that never publishes a result. The window starts when the
	// source pod becomes terminal, not when the checkpoint starts, so it is
	// separate from the agent's overall checkpoint timeout
	// (config.checkpoint.timeoutSeconds). It must cover the agent's upload of
	// the largest snapshot.
	CompletionGrace time.Duration
}

func (c CheckpointConfig) Validate() error {
	if c.CompletionGrace <= 0 {
		return fmt.Errorf("checkpoint completion grace must be positive, got %v", c.CompletionGrace)
	}
	return nil
}

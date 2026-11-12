// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCheckpointConfigValidate(t *testing.T) {
	require.NoError(t, CheckpointConfig{CompletionGrace: DefaultCheckpointCompletionGrace}.Validate())
	require.NoError(t, CheckpointConfig{CompletionGrace: time.Second}.Validate())
	for _, grace := range []time.Duration{0, -time.Minute} {
		require.ErrorContains(t, CheckpointConfig{CompletionGrace: grace}.Validate(), "must be positive")
	}
}

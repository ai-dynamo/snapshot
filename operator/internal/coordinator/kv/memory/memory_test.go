// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package memory_test

import (
	"testing"

	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv"
	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv/kvtest"
	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv/memory"
)

func TestConformance(t *testing.T) {
	kvtest.Run(t, func(*testing.T) kv.Store { return memory.New() })
}

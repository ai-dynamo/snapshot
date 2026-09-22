// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package backends_test

import (
	"testing"

	"github.com/ai-dynamo/snapshot/operator/internal/maintenance/backends"
	"github.com/stretchr/testify/assert"
)

func TestPVCBackendName(t *testing.T) {
	assert.Equal(t, "PVC", backends.NewPVCBackend("").Name())
}

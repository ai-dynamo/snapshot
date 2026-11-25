// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLabels(t *testing.T) {
	want := map[string]string{
		"app.kubernetes.io/part-of":    "snapshot",
		"app.kubernetes.io/managed-by": "snapshot-operator",
	}
	assert.Equal(t, want, Labels())

	changed := Labels()
	changed["other"] = "value"
	assert.Equal(t, want, Labels(), "a caller's change must not reach the next call")
}

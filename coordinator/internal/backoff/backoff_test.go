// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package backoff

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestNew covers the waits between tries: they double from a tenth of a second
// up to a second, which every later wait keeps, and each is lengthened at
// random by less than a tenth of it.
func TestNew(t *testing.T) {
	b := New()
	for i, want := range []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond,
		800 * time.Millisecond, time.Second, time.Second, time.Second} {
		got := b.Step()
		assert.GreaterOrEqual(t, got, want, "wait %d", i+1)
		assert.Less(t, got, want+want/10, "wait %d", i+1)
	}
}

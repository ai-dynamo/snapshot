// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package backoff spaces the tries of an operation that the session client or
// the coordinator sends again.
package backoff

import (
	"math"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
)

// The waits between the tries of an operation.
const (
	// first is the wait before the second try.
	first = 100 * time.Millisecond

	// factor multiplies each wait, up to longest.
	factor = 2

	// longest is the longest wait, which every wait after it keeps.
	longest = time.Second

	// jitter lengthens each wait at random by up to this fraction of it, so
	// that clients that fail together do not try again together.
	jitter = 0.1
)

// New returns the waits between the tries of an operation: first, then factor
// times longer each time, up to longest, each lengthened at random by up to
// jitter. Step returns the next wait.
func New() wait.Backoff {
	return wait.Backoff{Duration: first, Factor: factor, Jitter: jitter, Steps: math.MaxInt, Cap: longest}
}

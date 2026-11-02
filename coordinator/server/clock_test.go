// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"testing"
	"time"

	"k8s.io/utils/clock"
	clocktesting "k8s.io/utils/clock/testing"
)

// testClock is a fake clock that behaves as the real one does: it runs an
// AfterFunc callback on a goroutine of its own, never inside Step under the
// fake's lock. It reports every callback it arms, so a test can advance time
// only once the code under test is waiting on one; a report never blocks the
// code under test.
type testClock struct {
	*clocktesting.FakeClock
	timers chan time.Duration
}

// newTestClock returns a testClock that starts at start.
func newTestClock(start time.Time) *testClock {
	return &testClock{FakeClock: clocktesting.NewFakeClock(start), timers: make(chan time.Duration, 64)}
}

// AfterFunc arms f, to run on a goroutine of its own, and reports it.
func (c *testClock) AfterFunc(d time.Duration, f func()) clock.Timer {
	timer := c.FakeClock.AfterFunc(d, func() { go f() })
	select {
	case c.timers <- d:
	default:
	}
	return timer
}

// AwaitTimer blocks until the code under test has armed a callback, and
// returns its duration.
func (c *testClock) AwaitTimer(t testing.TB) time.Duration {
	t.Helper()
	select {
	case d := <-c.timers:
		return d
	case <-time.After(10 * time.Second):
		t.Fatal("no timer was armed")
		return 0
	}
}

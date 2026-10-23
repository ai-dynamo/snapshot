// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package clocktest is the fake clock the coordinator's tests share. Only tests
// import it.
package clocktest

import (
	"testing"
	"time"

	"k8s.io/utils/clock"
	clocktesting "k8s.io/utils/clock/testing"
)

// awaitLimit bounds how long AwaitTimer waits before failing the test. It is a
// safeguard against a hang, never a delay the test relies on.
const awaitLimit = 10 * time.Second

// Clock is a fake clock that reports every timer and every AfterFunc it hands
// out, so a test can advance time only once the code under test is waiting on
// one. Without it, stepping the clock before the timer exists would be lost,
// and waiting for the timer to exist would mean sleeping. A report never blocks
// the code under test, so a test that leaves reports unread only loses the
// ones it was not waiting for.
type Clock struct {
	*clocktesting.FakeClock
	timers chan time.Duration
}

// New returns a Clock that starts at start.
func New(start time.Time) *Clock {
	return &Clock{FakeClock: clocktesting.NewFakeClock(start), timers: make(chan time.Duration, 64)}
}

// NewTimer arms a timer and reports it.
func (c *Clock) NewTimer(d time.Duration) clock.Timer {
	timer := c.FakeClock.NewTimer(d)
	c.report(d)
	return timer
}

// AfterFunc arms a callback and reports it.
func (c *Clock) AfterFunc(d time.Duration, f func()) clock.Timer {
	timer := c.FakeClock.AfterFunc(d, f)
	c.report(d)
	return timer
}

// AwaitTimer blocks until the code under test has armed a timer, and returns
// its duration.
func (c *Clock) AwaitTimer(t testing.TB) time.Duration {
	t.Helper()
	select {
	case d := <-c.timers:
		return d
	case <-time.After(awaitLimit):
		t.Fatal("no timer was armed")
		return 0
	}
}

func (c *Clock) report(d time.Duration) {
	select {
	case c.timers <- d:
	default:
	}
}

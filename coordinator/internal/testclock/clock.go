// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package testclock is the fake clock of the coordinator's tests, which both
// the coordinator and the session client take as their Config.Clock.
package testclock

import (
	"sync"
	"testing"
	"time"

	"k8s.io/utils/clock"
	clocktesting "k8s.io/utils/clock/testing"
)

// Clock is a fake clock that behaves as the real one does: it runs an
// AfterFunc callback on a goroutine of its own, never inside Step under the
// fake's lock, and it fires a timer of zero or less at once. It counts the
// timers armed of each duration, so a test can advance time only once the
// code under test is waiting on one.
type Clock struct {
	*clocktesting.FakeClock

	mu    sync.Mutex
	armed map[time.Duration]int
}

// New returns a Clock that starts at start.
func New(start time.Time) *Clock {
	return &Clock{FakeClock: clocktesting.NewFakeClock(start), armed: make(map[time.Duration]int)}
}

// AfterFunc arms f, to run on a goroutine of its own, and counts it.
func (c *Clock) AfterFunc(d time.Duration, f func()) clock.Timer {
	return c.arm(c.FakeClock.AfterFunc(d, func() { go f() }), d)
}

// NewTimer arms a timer, and counts it.
func (c *Clock) NewTimer(d time.Duration) clock.Timer {
	return c.arm(c.FakeClock.NewTimer(d), d)
}

// arm counts a timer of duration d, and fires it at once if d is zero or
// less, which the fake does only once time moves.
func (c *Clock) arm(t clock.Timer, d time.Duration) clock.Timer {
	c.mu.Lock()
	c.armed[d]++
	c.mu.Unlock()
	if d <= 0 {
		c.Step(0)
	}
	return t
}

// Await blocks until the code under test has armed a timer of duration d that
// no earlier Await returned for.
func (c *Clock) Await(t testing.TB, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		c.mu.Lock()
		armed := c.armed[d] > 0
		if armed {
			c.armed[d]--
		}
		c.mu.Unlock()
		switch {
		case armed:
			return
		case time.Now().After(deadline):
			t.Fatalf("no %v timer was armed", d)
		}
		time.Sleep(time.Millisecond)
	}
}

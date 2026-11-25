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
// timers armed, and stopped before they fired, of each duration, so a test can
// advance time only once the code under test is waiting on one, or has stopped
// waiting.
type Clock struct {
	*clocktesting.FakeClock

	mu             sync.Mutex
	armed, stopped map[time.Duration]int
}

// New returns a Clock that starts at start.
func New(start time.Time) *Clock {
	return &Clock{
		FakeClock: clocktesting.NewFakeClock(start),
		armed:     make(map[time.Duration]int),
		stopped:   make(map[time.Duration]int),
	}
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
	c.count(c.armed, d)
	if d <= 0 {
		c.Step(0)
	}
	return &timer{Timer: t, c: c, d: d}
}

// Armed returns how many timers of duration d have been armed that no Await
// returned for.
func (c *Clock) Armed(d time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.armed[d]
}

// Await blocks until the code under test has armed a timer of duration d that
// no earlier Await returned for.
func (c *Clock) Await(t testing.TB, d time.Duration) {
	t.Helper()
	c.await(t, c.armed, d, "armed")
}

// AwaitStop blocks until the code under test has stopped a timer of duration
// d before it fired, that no earlier AwaitStop returned for.
func (c *Clock) AwaitStop(t testing.TB, d time.Duration) {
	t.Helper()
	c.await(t, c.stopped, d, "stopped")
}

func (c *Clock) count(counts map[time.Duration]int, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	counts[d]++
}

func (c *Clock) await(t testing.TB, counts map[time.Duration]int, d time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		c.mu.Lock()
		found := counts[d] > 0
		if found {
			counts[d]--
		}
		c.mu.Unlock()
		switch {
		case found:
			return
		case time.Now().After(deadline):
			t.Fatalf("no %v timer was %s", d, what)
		}
		time.Sleep(time.Millisecond)
	}
}

// timer counts a stop that keeps its callback from running.
type timer struct {
	clock.Timer
	c *Clock
	d time.Duration
}

// Stop implements clock.Timer.
func (t *timer) Stop() bool {
	stopped := t.Timer.Stop()
	if stopped {
		t.c.count(t.c.stopped, t.d)
	}
	return stopped
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/session"
)

// controlWatcher records arrivals and the session's end from one ordered watch.
// Waiters retain no state here: only the barriers' arrivals, by member, remain.
// A recorded end freezes those arrivals before the connection closes.
type controlWatcher struct {
	s *client

	mu       sync.Mutex
	arrivals map[string]map[string]struct{}
	changed  chan struct{}
	err      error
}

func newControlWatcher(s *client) *controlWatcher {
	return &controlWatcher{s: s, arrivals: make(map[string]map[string]struct{}), changed: make(chan struct{})}
}

// run follows every barrier and ended together. A watch that stops or fails is
// replaced while the session remains live; it never leaves the end unwatched.
func (c *controlWatcher) run() {
	defer c.s.wg.Done()
	keys := []string{session.BarrierKey("*", "*"), session.KeyEnded}
	for {
		err := c.s.await(context.Background(), c.s.control.kv, keys, c.record)
		if err == nil || c.s.alive() != nil {
			return
		}
		c.s.log.Error(err, "Watching the session's control keys failed; watching again")
		select {
		case <-time.After(attemptTimeout):
		case <-c.s.closing:
			return
		case <-c.s.done:
			return
		}
	}
}

// record verifies the session before accepting an entry, so a reconnected
// watch cannot report arrivals from a session the coordinator recreated.
func (c *controlWatcher) record(key string, value []byte, present bool) bool {
	if err := c.s.settle(context.Background()); err != nil {
		return true
	}
	if key == session.KeyEnded {
		if present {
			c.s.finish(session.EndedError(value))
			return true
		}
		return false
	}
	parts := strings.Split(key, ".")
	if len(parts) != 3 || session.ValidateName(parts[1]) != nil {
		return false
	}
	name, member := parts[1], parts[2]
	if _, ok := c.s.ranks[member]; !ok {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return true
	}
	arrived := c.arrivals[name]
	if present {
		if _, ok := arrived[member]; ok {
			return false
		}
		if arrived == nil {
			arrived = make(map[string]struct{})
			c.arrivals[name] = arrived
		}
		arrived[member] = struct{}{}
	} else {
		if _, ok := arrived[member]; !ok {
			return false
		}
		delete(arrived, member)
		if len(arrived) == 0 {
			delete(c.arrivals, name)
		}
	}
	close(c.changed)
	c.changed = make(chan struct{})
	return false
}

// end freezes the ordered evidence and wakes every waiter. Local deadline and
// coordinator-loss paths also call it through finish.
func (c *controlWatcher) end(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.err = err
		close(c.changed)
	}
}

// wait reports a release recorded before the end, even if the end woke the
// caller first. Otherwise it returns the end's reason or the caller's error.
func (c *controlWatcher) wait(ctx context.Context, name string) error {
	for {
		c.mu.Lock()
		released := len(c.arrivals[name]) == len(c.s.ranks)
		err, changed := c.err, c.changed
		c.mu.Unlock()
		if released {
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		case <-c.s.closing:
			return net.ErrClosed
		}
	}
}

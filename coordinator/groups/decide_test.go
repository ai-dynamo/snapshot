// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package groups

import (
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/stretchr/testify/assert"
)

func TestDecide(t *testing.T) {
	const node = "node-0"
	deadline := created.Add(10 * time.Minute)
	now := created.Add(time.Minute)
	// served returns an unfinished attempt this node serves, whose member work
	// exists, changed by change.
	served := func(change func(*attempt)) *attempt {
		a := &attempt{spec: server.Spec{UID: "uid", Deadline: deadline}, sessionNode: node, memberWork: true}
		if change != nil {
			change(a)
		}
		return a
	}
	final := func(a *attempt) { a.final = true }
	deleting := func(a *attempt) { a.deleting = true }
	var (
		notHeld = held{}
		open    = held{session: true}
		ended   = held{session: true, ended: true}
	)
	tests := []struct {
		name    string
		attempt *attempt
		held    held
		now     time.Time
		action  action
		again   time.Duration
	}{
		{"gone, its session open", nil, open, now, remove, 0},
		{"gone, its session ended", nil, ended, now, remove, 0},
		{"gone, no session", nil, notHeld, now, none, 0},

		{"unfinished, served here, member work", served(nil), notHeld, now, ensure, 0},
		{"unfinished past its deadline, served here, member work", served(nil), notHeld, deadline.Add(time.Second),
			ensure, 0},
		{"unfinished, its session open", served(nil), open, now, none, 0},
		{"unfinished, its session ended", served(nil), ended, now, none, 0},
		{"unfinished, served by another node", served(func(a *attempt) { a.sessionNode = "node-1" }), notHeld, now,
			none, 0},
		{"unfinished, the rank-0 member's node not known yet", served(func(a *attempt) { a.sessionNode = "" }),
			notHeld, now, none, 0},
		{"unfinished, no member work", served(func(a *attempt) { a.memberWork = false }), notHeld, now, none, 0},

		{"final, no session", served(final), notHeld, now, none, 0},
		{"final, its session ended", served(final), ended, now, remove, 0},
		{"final, its session open", served(final), open, now, none, recheck},
		{"final, its session open, its deadline sooner than a recheck", served(final), open,
			deadline.Add(-2 * time.Second), none, 2 * time.Second},
		{"final, its session open at its deadline", served(final), open, deadline, remove, 0},
		{"final, its session open past its deadline", served(final), open, deadline.Add(time.Second), remove, 0},

		{"deleting, its session open", served(deleting), open, now, cancel, 0},
		{"deleting, its session ended", served(deleting), ended, now, none, 0},
		{"deleting, no session", served(deleting), notHeld, now, none, 0},
		{"deleting and final, its session open", served(func(a *attempt) { final(a); deleting(a) }), open, now,
			cancel, recheck},
		{"deleting and final, its session ended", served(func(a *attempt) { final(a); deleting(a) }), ended, now,
			remove, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			act, again := decide(tt.attempt, node, tt.held, tt.now)
			assert.Equal(t, tt.action, act, "action")
			assert.Equal(t, tt.again, again, "again")
		})
	}
}

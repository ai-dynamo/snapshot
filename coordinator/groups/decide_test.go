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
		a := &attempt{spec: server.Spec{UID: "uid", Deadline: deadline}, ensurable: true, sessionNode: node,
			memberWork: true}
		if change != nil {
			change(a)
		}
		return a
	}
	final := func(a *attempt) { a.final = true }
	deleting := func(a *attempt) { a.deleting = true }
	failed := func(a *attempt) { a.failed = true }
	// unranked is a restore whose PodGroupSnapshot is gone.
	unranked := func(a *attempt) { a.ensurable = false }
	both := func(changes ...func(*attempt)) func(*attempt) {
		return func(a *attempt) {
			for _, change := range changes {
				change(a)
			}
		}
	}
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
		{"deleting, no session", served(deleting), notHeld, now, ensure, 0},
		{"deleting and final, its session open", served(both(final, deleting)), open, now, cancel, recheck},
		{"deleting and final, its session ended", served(both(final, deleting)), ended, now, remove, 0},

		{"a member failed, its session open", served(failed), open, now, fail, 0},
		{"a member failed, its session ended", served(failed), ended, now, none, 0},
		{"a member failed, no session, served here, member work", served(failed), notHeld, now, ensure, 0},
		{"a member failed, no session, served by another node",
			served(both(failed, func(a *attempt) { a.sessionNode = "node-1" })), notHeld, now, none, 0},
		{"a member failed and final, its session open", served(both(failed, final)), open, now, fail, recheck},
		{"a member failed and final, its session ended", served(both(failed, final)), ended, now, remove, 0},
		{"a member failed and deleting, its session open", served(both(failed, deleting)), open, now, cancel, 0},

		{"not ensurable, served here, member work", served(unranked), notHeld, now, none, 0},
		{"not ensurable, its session open", served(unranked), open, now, none, 0},
		{"not ensurable and final, its session open", served(both(unranked, final)), open, now, none, recheck},
		{"not ensurable and final, its session ended", served(both(unranked, final)), ended, now, remove, 0},
		{"not ensurable and final, its session open past its deadline", served(both(unranked, final)), open,
			deadline.Add(time.Second), remove, 0},
		{"not ensurable and deleting, its session open", served(both(unranked, deleting)), open, now, cancel, 0},
		{"not ensurable, a member failed, its session open", served(both(unranked, failed)), open, now, fail, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			act, again := decide(tt.attempt, node, tt.held, tt.now)
			assert.Equal(t, tt.action, act, "action")
			assert.Equal(t, tt.again, again, "again")
		})
	}
}

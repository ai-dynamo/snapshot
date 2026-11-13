// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package groups

import "time"

// action is what to do to an attempt's session.
type action int

const (
	none action = iota
	ensure
	cancel
	remove
)

// String implements fmt.Stringer.
func (a action) String() string {
	switch a {
	case ensure:
		return "ensure"
	case cancel:
		return "cancel"
	case remove:
		return "remove"
	}
	return "none"
}

// held is what the coordinator holds of an attempt's session, as
// server.Server.Ended reports it.
type held struct {
	// session reports whether the coordinator holds the session, and ended
	// whether it has ended.
	session, ended bool
}

// recheck is how long a final attempt whose session is still open waits before
// it is looked at again. Nothing on the group resource changes when the
// session ends, so the reconciler looks for it: a capture's session closes
// once every member has left, soon after the attempt is final, while a
// restore's stays open past release for as long as the members' libraries
// rendezvous through it.
const recheck = 5 * time.Second

// decide returns what to do to the session of attempt a, on the coordinator of
// node, which holds h of it, at now, and how long until the reconciler should
// look again, or zero for no sooner than the next change. a is nil when the
// group resource is gone, when another UID holds its name, or when it is a
// restore that has no attempt. The first row that matches wins:
//
//	remove  a is nil, and the coordinator holds the session
//	remove  a is final, and its session has ended or its deadline has passed
//	cancel  a's deletion has started, and its session is still open
//	ensure  node runs a's rank-0 member, member work exists, a is neither
//	        final nor being deleted, and the coordinator holds no session
//	none    otherwise
//
// A final attempt whose session is still open is looked at again after
// recheck, or at its deadline if that is sooner. Ensure never revives a
// removed session: a resource is final or being deleted, or gone, before its
// session is removed, and stays so, since a UID names one attempt.
func decide(a *attempt, node string, h held, now time.Time) (action, time.Duration) {
	if a == nil {
		if h.session {
			return remove, 0
		}
		return none, 0
	}
	if a.final && h.session && (h.ended || !now.Before(a.spec.Deadline)) {
		return remove, 0
	}
	open := h.session && !h.ended
	var again time.Duration
	if a.final && open {
		again = min(recheck, a.spec.Deadline.Sub(now))
	}
	switch {
	case a.deleting && open:
		return cancel, again
	case a.sessionNode == node && a.memberWork && !a.final && !a.deleting && !h.session:
		return ensure, 0
	}
	return none, again
}

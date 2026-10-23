// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"fmt"
	"sync"
)

// ledger is everything a session records beside its space: how it ended, which
// members have left, the outcome of every request a caller may retry, and what
// library callers hold against their quotas.
//
// It is kept in one type because it is all of a session's state apart from the
// space itself. A persistent backend would have to persist it with the space,
// and keeping it together means it can move behind the store without the rest
// of the session noticing. The Epoch would then have to identify the generation
// of that state rather than the process, since a restarted coordinator would
// hold the same sessions.
type ledger struct {
	mu sync.Mutex
	// ended is nil while the session is open and holds the terminal error —
	// ErrClosed, or an *AbortError naming the reason — once it is not.
	ended           error
	left            map[MemberID]struct{}
	requests        map[dedupKey]*outcome
	libraryRequests int

	// usage has its own mutex, held across a library write; it is never
	// taken together with mu.
	usage usage
}

// dedupKey scopes a request identifier to the caller that issued it, so two
// callers that choose the same identifier never read each other's outcome.
type dedupKey struct {
	caller Caller
	id     RequestID
}

// outcome is the result of one request a caller may retry. done is closed once
// value and err are set, so a retry that arrives while the request is still
// running waits for its outcome rather than applying it a second time.
type outcome struct {
	done  chan struct{}
	value int64
	err   error
}

// end records the session's terminal error once, and reports whether this call
// did, so a member and the deadline racing each other end the session once.
func (l *ledger) end(err error) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ended != nil {
		return false
	}
	l.ended = err
	return true
}

// recallLocked returns the outcome recorded for r, and whether one was. For a
// new request it records a pending outcome, which a library caller may do only
// within MaxLibraryRequests. The caller holds mu.
func (l *ledger) recallLocked(r Request) (*outcome, bool, error) {
	key := dedupKey{caller: r.Caller, id: r.ID}
	if recorded, ok := l.requests[key]; ok {
		return recorded, true, nil
	}
	if r.Caller.Role == RoleLibrary {
		if l.libraryRequests >= MaxLibraryRequests {
			return nil, false, fmt.Errorf("%d library requests recorded: %w", l.libraryRequests, ErrTooMany)
		}
		l.libraryRequests++
	}
	pending := &outcome{done: make(chan struct{})}
	l.requests[key] = pending
	return pending, false, nil
}

// reset empties everything only an open session needs, keeping how it ended.
func (l *ledger) reset() {
	l.mu.Lock()
	l.left = make(map[MemberID]struct{})
	l.requests = make(map[dedupKey]*outcome)
	l.libraryRequests = 0
	l.mu.Unlock()
	l.usage.reset()
}

// set records the outcome and releases every retry waiting for it. A request
// without an identifier has no outcome to record, so set on nil does nothing.
func (o *outcome) set(value int64, err error) {
	if o == nil {
		return
	}
	o.value, o.err = value, err
	close(o.done)
}

// wait returns the outcome once it is set, or ctx's error as soon as ctx is
// done.
func (o *outcome) wait(ctx context.Context) (int64, error) {
	select {
	case <-o.done:
		return o.value, o.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

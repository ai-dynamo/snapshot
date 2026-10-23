// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/session"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
)

// memberKey identifies one member of one attempt.
type memberKey struct {
	uid    types.UID
	member session.MemberID
}

// registry tracks the stream each member is attached through, and the grace
// period of each member whose stream was lost.
//
// Its mutex guards both maps and nothing else. It is never held while calling
// the manager, a session, or the clock: the clock may run a grace callback,
// which takes the mutex, under a lock of its own. A registry holds nothing for
// a session once it has ended and its streams have returned, except a grace
// period that began before the session ended with no stream left to see the
// end; that one finds the session ended when it expires, within the grace
// period, and goes.
type registry struct {
	manager *session.Manager
	clock   clock.WithDelayedExecution
	grace   time.Duration
	log     logr.Logger

	// arrivals numbers streams as their Hellos arrive.
	arrivals atomic.Uint64

	mu      sync.Mutex
	streams map[memberKey]*stream
	graces  map[memberKey]*grace
}

// grace is one member's grace period, which began when the stream numbered seq
// was lost. timer is nil until lose records it.
type grace struct {
	seq   uint64
	timer clock.Timer
}

func newRegistry(manager *session.Manager, cl clock.WithDelayedExecution, period time.Duration,
	log logr.Logger) *registry {
	return &registry{
		manager: manager,
		clock:   cl,
		grace:   period,
		log:     log,
		streams: make(map[memberKey]*stream),
		graces:  make(map[memberKey]*grace),
	}
}

// arrive numbers a stream whose Hello the server just read. Numbers grow in the
// order Hellos arrive, which decides which of a member's streams is the newest.
func (r *registry) arrive() uint64 {
	return r.arrivals.Add(1)
}

// register makes st the member's stream, unless a newer stream got there first:
// one attached, or one lost whose grace period is running. It then ends the
// member's grace period, if one is running, supersedes the member's older
// stream, if one is attached, and reports true.
func (r *registry) register(st *stream) bool {
	key := st.key()
	r.mu.Lock()
	older, lost := r.streams[key], r.graces[key]
	if (older != nil && older.seq > st.seq) || (lost != nil && lost.seq > st.seq) {
		r.mu.Unlock()
		return false
	}
	r.streams[key] = st
	var timer clock.Timer
	if lost != nil {
		timer = lost.timer
		delete(r.graces, key)
	}
	r.mu.Unlock()

	// A timer lose has not recorded yet is stopped by lose itself, once it
	// finds its grace period gone.
	if timer != nil {
		timer.Stop()
	}
	if older != nil {
		older.supersede()
	}
	return true
}

// lose starts the grace period of a member whose stream ended before Ended and
// was not superseded. The timer is armed outside the mutex, so the period is
// recorded first and the timer after: a stream that attaches in between ends
// the period, and lose then stops the timer it armed.
func (r *registry) lose(st *stream) {
	key := st.key()
	g := &grace{seq: st.seq}
	r.mu.Lock()
	if r.streams[key] != st {
		// A newer stream attached before this one was lost.
		r.mu.Unlock()
		return
	}
	delete(r.streams, key)
	r.graces[key] = g
	r.mu.Unlock()

	r.log.V(1).Info("Member's stream lost; waiting for it to reattach", "attempt", key.uid, "member", key.member)
	timer := r.clock.AfterFunc(r.grace, func() { r.expire(key, g) })
	r.mu.Lock()
	running := r.graces[key] == g
	if running {
		g.timer = timer
	}
	r.mu.Unlock()
	if !running {
		timer.Stop()
	}
}

// ended forgets a stream that sent Ended, and every grace period of its
// session, which can no longer fail anything.
func (r *registry) ended(st *stream) {
	key := st.key()
	var timers []clock.Timer
	r.mu.Lock()
	if r.streams[key] == st {
		delete(r.streams, key)
	}
	for k, g := range r.graces {
		if k.uid == key.uid {
			delete(r.graces, k)
			if g.timer != nil {
				timers = append(timers, g.timer)
			}
		}
	}
	r.mu.Unlock()

	for _, timer := range timers {
		timer.Stop()
	}
}

// expire ends a grace period nobody ended. It is the timer's callback, so it
// cannot stop its own timer, and need not, since the timer has fired.
//
// It fails the member's session from a goroutine of its own rather than in the
// callback: failing a session stops the session's deadline timer, and a clock
// may run its callbacks under the very lock that Stop takes. The goroutine
// lives only as long as Fail, which returns as soon as the session's calls
// have.
func (r *registry) expire(key memberKey, g *grace) {
	r.mu.Lock()
	current := r.graces[key] == g
	if current {
		delete(r.graces, key)
	}
	r.mu.Unlock()
	if !current {
		return
	}

	r.log.Info("A member's stream was lost and no new one attached within the grace period; failing the session",
		"attempt", key.uid, "member", key.member)
	go func() {
		// An error means the session ended, or was removed, meanwhile, with
		// nothing left to fail.
		_ = r.manager.Fail(context.Background(), key.uid, session.ReasonMemberFailed)
	}()
}

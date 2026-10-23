// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv/memory"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	clocktesting "k8s.io/utils/clock/testing"
)

// The fixtures every test shares: one attempt, two members, and keys inside
// each role's namespace.
const (
	attemptUID  = types.UID("attempt-1")
	otherUID    = types.UID("attempt-2")
	memberA     = MemberID("member-a")
	memberB     = MemberID("member-b")
	memberC     = MemberID("member-c")
	agentKey    = agentNamespace + "handles"
	agentOther  = agentNamespace + "ranks"
	relayKey    = relayNamespace + "command"
	libraryKey  = libraryNamespace + "nccl/2.28/bootstrap"
	ncclKey     = ncclNamespace + "bootstrap"
	ncclOther   = ncclNamespace + "rendezvous"
	barrierName = agentNamespace + "admitted"
	deadlineIn  = time.Minute

	failureReason = Reason("MemberFailed")
)

// baseTime is where every fake clock starts, so a spec's deadline and the
// session's clock agree without either having to be built first.
var baseTime = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

// agentOf, relayOf, and ncclOf return the callers a member's agent, its relay,
// and its NCCL Checkpoint compatibility clients make their requests under.
func agentOf(member MemberID) Caller {
	return Caller{Member: member, Role: RoleAgent}
}

func relayOf(member MemberID) Caller {
	return Caller{Member: member, Role: RoleRelay}
}

func ncclOf(member MemberID) Caller {
	return Caller{Member: member, Role: RoleLibrary, Library: LibraryNCCL}
}

// requestOf returns one agent request, with an identifier only when asked for.
func requestOf(member MemberID, id RequestID) Request {
	return Request{Caller: agentOf(member), ID: id}
}

// specOf returns a spec whose members hold consecutive ranks, defaulting to two
// members.
func specOf(uid types.UID, members ...MemberID) Spec {
	if len(members) == 0 {
		members = []MemberID{memberA, memberB}
	}
	spec := Spec{UID: uid, Deadline: baseTime.Add(deadlineIn)}
	for rank, id := range members {
		spec.Members = append(spec.Members, Member{ID: id, Rank: rank})
	}
	return spec
}

// signalClock reports every timer and every AfterFunc it hands out, so a test
// can advance time only once the call under test is waiting on one. Without
// it, stepping the clock before the timer exists would be lost, and waiting
// for it to exist would mean sleeping. The report never blocks the code under
// test, so a test that leaves timers undrained only loses a signal it was not
// waiting for.
type signalClock struct {
	*clocktesting.FakeClock
	timers chan time.Duration
}

func newSignalClock() *signalClock {
	return &signalClock{
		FakeClock: clocktesting.NewFakeClock(baseTime),
		timers:    make(chan time.Duration, 64),
	}
}

func (c *signalClock) NewTimer(d time.Duration) clock.Timer {
	timer := c.FakeClock.NewTimer(d)
	c.signal(d)
	return timer
}

func (c *signalClock) AfterFunc(d time.Duration, f func()) clock.Timer {
	timer := c.FakeClock.AfterFunc(d, f)
	c.signal(d)
	return timer
}

func (c *signalClock) signal(d time.Duration) {
	select {
	case c.timers <- d:
	default:
	}
}

// awaitTimer blocks until the code under test has armed a timer.
func (c *signalClock) awaitTimer(t *testing.T) {
	t.Helper()
	select {
	case <-c.timers:
	case <-time.After(10 * time.Second):
		t.Fatal("no timer was armed")
	}
}

// operation is one OperationLatency report, with everything a metric may label.
type operation struct {
	role Role
	op   Operation
}

// recordingObserver keeps what sessions reported. Its methods have exactly the
// parameters Observer gives them, so it records everything a metric could
// label, and a test can check that no name ever reaches it.
type recordingObserver struct {
	mu           sync.Mutex
	opened       []types.UID
	closed       []types.UID
	failed       []Reason
	operations   []operation
	keyWaits     []Role
	barrierWaits []Role
}

func (o *recordingObserver) SessionOpened(uid types.UID) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.opened = append(o.opened, uid)
}

func (o *recordingObserver) SessionClosed(uid types.UID) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = append(o.closed, uid)
}

func (o *recordingObserver) SessionFailed(_ types.UID, reason Reason) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.failed = append(o.failed, reason)
}

func (o *recordingObserver) OperationLatency(_ types.UID, role Role, op Operation, _ time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.operations = append(o.operations, operation{role: role, op: op})
}

func (o *recordingObserver) KeyWaited(_ types.UID, role Role, _ time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.keyWaits = append(o.keyWaits, role)
}

func (o *recordingObserver) BarrierWaited(_ types.UID, role Role, _ time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.barrierWaits = append(o.barrierWaits, role)
}

// snapshot returns a copy of what was recorded so far.
func (o *recordingObserver) snapshot() recordingObserver {
	o.mu.Lock()
	defer o.mu.Unlock()
	return recordingObserver{
		opened:       append([]types.UID(nil), o.opened...),
		closed:       append([]types.UID(nil), o.closed...),
		failed:       append([]Reason(nil), o.failed...),
		operations:   append([]operation(nil), o.operations...),
		keyWaits:     append([]Role(nil), o.keyWaits...),
		barrierWaits: append([]Role(nil), o.barrierWaits...),
	}
}

// fixture is one Manager over an in-memory store, usually with one session.
type fixture struct {
	manager  *Manager
	store    *memory.Store
	clock    *signalClock
	observer *recordingObserver
	session  *Session
}

// newFixture ensures a session for attemptUID with the given members,
// defaulting to two.
func newFixture(t *testing.T, members ...MemberID) *fixture {
	t.Helper()
	f := newManagerFixture(t)
	f.session = f.ensure(t, specOf(attemptUID, members...))
	return f
}

// newManagerFixture builds a Manager without ensuring any session.
func newManagerFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{store: memory.New(), clock: newSignalClock(), observer: &recordingObserver{}}
	manager, err := NewManager(Config{Store: f.store, Clock: f.clock, Observer: f.observer})
	require.NoError(t, err)
	f.manager = manager
	return f
}

// ensure creates a session and consumes the signal of the deadline it armed.
func (f *fixture) ensure(t *testing.T, spec Spec) *Session {
	t.Helper()
	s, err := f.manager.Ensure(spec)
	require.NoError(t, err)
	if spec.Deadline.After(f.clock.Now()) {
		f.clock.awaitTimer(t)
	}
	return s
}

// stored reads key straight from the store, past the session, to show what
// the session left there.
func (f *fixture) stored(uid types.UID, key string) ([]byte, error) {
	return f.store.Space(uid).Get(context.Background(), key)
}

// await returns what a blocking call reported, failing the test if it never
// returns.
func await[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatal("a blocking call never returned")
		panic("unreachable")
	}
}

// getAsync starts a Get with a wait and returns where its error arrives, once
// the Get has armed its wait.
func getAsync(t *testing.T, f *fixture, c Caller, key string, wait time.Duration) <-chan error {
	t.Helper()
	got := make(chan error, 1)
	go func() {
		_, err := f.session.Get(context.Background(), c, key, wait)
		got <- err
	}()
	f.clock.awaitTimer(t)
	return got
}

// barrierAsync starts a AwaitBarrier and returns where its error arrives.
func barrierAsync(s *Session, c Caller, name string) <-chan error {
	got := make(chan error, 1)
	go func() { got <- s.AwaitBarrier(context.Background(), c, name) }()
	return got
}

// assertAbortedWith reports that an error is the session's abort with reason.
func assertAbortedWith(t *testing.T, err error, reason Reason) {
	t.Helper()
	require.ErrorIs(t, err, ErrAborted)
	var aborted *AbortError
	require.ErrorAs(t, err, &aborted)
	require.Equal(t, reason, aborted.Reason)
}

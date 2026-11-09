// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package sessiontest holds only tests: those of session behavior, through the
// session client, package coordinator, against the real coordinator, package
// server, and the test that ties the client's connection settings to the
// coordinator's. It is the only package that imports both. One fake clock
// times both sides: the coordinator's deadlines and grace periods, and each
// member's own deadline. Each member reaches the coordinator through a TCP
// proxy of its own, which a test cuts to break the member's connection, and
// points at a new coordinator to restart it. What a misbehaving connection can
// do is package server's to test, over raw connections.
package sessiontest

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator"
	"github.com/ai-dynamo/snapshot/coordinator/coordinatortest"
	"github.com/ai-dynamo/snapshot/coordinator/internal/testcert"
	"github.com/ai-dynamo/snapshot/coordinator/internal/testclock"
	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"k8s.io/apimachinery/pkg/types"
)

// harness is a coordinator under test, with an Authorizer the test controls,
// the proxies its members connect through, and a fake clock that times both
// the coordinator's deadlines and grace periods and its members' deadlines.
type harness struct {
	t     *testing.T
	cfg   server.Config
	srv   *server.Server
	ca    *x509.CertPool
	auth  *coordinatortest.Authorizer
	clock *testclock.Clock
	// logs records what every member's client logs.
	logs *logLines
	// observer records what the current coordinator sees of sessions.
	observer *recorder
	proxies  []*proxy
}

// newHarness starts a ready coordinator, configured by modify, and checks
// that the test leaves no goroutine behind.
func newHarness(t *testing.T, modify ...func(*server.Config)) *harness {
	t.Helper()
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) })
	ca, cert := testcert.New(t)
	h := &harness{
		t: t, ca: ca, auth: &coordinatortest.Authorizer{},
		clock: testclock.New(time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)),
		logs:  &logLines{},
	}
	// Every subject a client sends must be one its permissions allow. The check
	// is best-effort: a refusal that races a closing connection can go unseen.
	// The deterministic tests of the subjects whose refusal changes nothing a
	// client returns are TestWaitsFreeTheirConsumers, for the deletion of a
	// watch's consumer, and package server's TestFlowControlReplies.
	t.Cleanup(func() {
		for _, line := range h.logs.all() {
			assert.NotContains(t, line, "refused a publish", "a client sent a subject it is not permitted")
		}
	})
	h.cfg = server.Config{
		Host:       "127.0.0.1",
		Port:       natsserver.RANDOM_PORT,
		TLS:        &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		Authorizer: h.auth,
		Clock:      h.clock,
	}
	for _, m := range modify {
		m(&h.cfg)
	}
	h.start()
	h.srv.MarkReady()
	return h
}

// start starts a coordinator, not ready yet, with an Observer of its own, and
// shuts it down when the test ends.
func (h *harness) start() {
	h.t.Helper()
	h.observer = &recorder{}
	h.cfg.Observer = h.observer
	srv, err := server.New(h.cfg)
	require.NoError(h.t, err)
	h.t.Cleanup(func() { require.NoError(h.t, srv.Shutdown(context.Background())) })
	h.srv = srv
}

// restart replaces the coordinator, as a restart of its host would: the new
// coordinator recreates the sessions of specs, with new epochs, before it
// admits anyone, and every member's connection breaks and reconnects to it.
// The old coordinator shuts down only once no member reaches it, so no member
// learns from it that it went away.
func (h *harness) restart(specs ...server.Spec) {
	h.t.Helper()
	old := h.srv
	h.start()
	for _, p := range h.proxies {
		p.retarget(h.srv.Addr().String())
		p.cut()
	}
	require.NoError(h.t, old.Shutdown(within(h.t)))
	for _, spec := range specs {
		require.NoError(h.t, h.srv.Ensure(within(h.t), spec))
	}
	h.srv.MarkReady()
}

// testSession is the session of one attempt, whose members are named member-0
// to member-<n-1>.
type testSession struct {
	h    *harness
	spec server.Spec
	// proxies holds each member's proxy.
	proxies []*proxy
}

// session ensures a session with n members, whose deadline is an hour away,
// for the test t, whose members' proxies stop when t ends.
func (h *harness) session(t *testing.T, n int) *testSession {
	t.Helper()
	return h.sessionUntil(t, n, h.clock.Now().Add(time.Hour))
}

// sessionUntil ensures a session with n members and deadline for the test t.
func (h *harness) sessionUntil(t *testing.T, n int, deadline time.Time) *testSession {
	t.Helper()
	ses := &testSession{h: h, spec: server.Spec{
		UID:      types.UID(fmt.Sprintf("attempt-%d", time.Now().UnixNano())),
		Deadline: deadline,
	}}
	for i := range n {
		ses.spec.Members = append(ses.spec.Members, session.Member{ID: fmt.Sprintf("member-%d", i), Rank: i})
		p := newProxy(t, h.srv.Addr().String())
		h.proxies = append(h.proxies, p)
		ses.proxies = append(ses.proxies, p)
	}
	require.NoError(t, h.srv.Ensure(within(t), ses.spec))
	return ses
}

// config returns the Config of one member, which reaches the coordinator
// through the member's proxy.
func (ses *testSession) config(member int) coordinator.Config {
	return coordinator.Config{
		URLs:       []string{"tls://" + ses.proxies[member].addr()},
		RootCAs:    func() (*x509.CertPool, error) { return ses.h.ca, nil },
		ServerName: "localhost",
		Attempt:    string(ses.spec.UID),
		Member:     ses.spec.Members[member].ID,
		Token:      func() (string, error) { return coordinatortest.Token, nil },
		Clock:      ses.h.clock,
		Log:        ses.h.logs.logger(),
	}
}

// trust is the CAs a member's client reads on every handshake, which a test
// replaces, as the kubelet refreshes a mounted ca.crt, or makes unreadable.
type trust struct {
	pool       atomic.Pointer[x509.CertPool]
	unreadable atomic.Bool
	reads      atomic.Int64
}

func newTrust(pool *x509.CertPool) *trust {
	tr := &trust{}
	tr.pool.Store(pool)
	return tr
}

// RootCAs implements coordinator.Config's RootCAs.
func (tr *trust) RootCAs() (*x509.CertPool, error) {
	tr.reads.Add(1)
	if tr.unreadable.Load() {
		return nil, errors.New("ca.crt is being replaced")
	}
	return tr.pool.Load(), nil
}

// connectTrusting connects one member, which reads its CAs from tr, and
// closes its session when the test t ends.
func (ses *testSession) connectTrusting(t *testing.T, member int, tr *trust) coordinator.Session {
	t.Helper()
	cfg := ses.config(member)
	cfg.RootCAs = tr.RootCAs
	s, err := coordinator.Connect(within(t), cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

// cutUntilRefused cuts a member's connections, and waits until its client,
// which reads its CAs from tr, has read them twice since: the handshake that
// read them first failed, and the client is trying again.
func (ses *testSession) cutUntilRefused(t *testing.T, member int, tr *trust) {
	t.Helper()
	reads := tr.reads.Load()
	ses.cut(member)
	require.Eventually(t, func() bool { return tr.reads.Load() >= reads+2 }, 10*time.Second, 5*time.Millisecond,
		"member-%d did not read its CAs again", member)
}

// connect connects one member, and closes its session when the test t ends.
func (ses *testSession) connect(t *testing.T, member int) coordinator.Session {
	t.Helper()
	s, err := coordinator.Connect(within(t), ses.config(member))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

// connectAll connects every member for the test t.
func (ses *testSession) connectAll(t *testing.T) []coordinator.Session {
	t.Helper()
	sessions := make([]coordinator.Session, len(ses.spec.Members))
	for i := range ses.spec.Members {
		sessions[i] = ses.connect(t, i)
	}
	return sessions
}

// in returns a member's Store of a namespace.
func in(s coordinator.Session, ns session.Namespace) coordinator.Store {
	switch ns {
	case session.NamespaceCuInterpose:
		return s.CuInterpose()
	case session.NamespaceLibrary:
		return s.Library()
	}
	return s.Snapshot()
}

// cut breaks every connection of a member, as a broken network would, so its
// client reconnects.
func (ses *testSession) cut(member int) { ses.proxies[member].cut() }

// raw returns the session's Snapshot bucket over a plain NATS connection of
// member, made the way package session defines, which reaches the keys the
// client keeps to itself. The connection closes when the test ends.
func (ses *testSession) raw(t *testing.T, member int) jetstream.KeyValue {
	t.Helper()
	return ses.rawBucket(t, member, session.SnapshotBucket(string(ses.spec.UID)))
}

// rawBucket returns one of the session's buckets over a plain NATS connection
// of member, as raw does.
func (ses *testSession) rawBucket(t *testing.T, member int, bucket string) jetstream.KeyValue {
	t.Helper()
	uid, id := string(ses.spec.UID), ses.spec.Members[member].ID
	nc, err := nats.Connect("tls://"+ses.h.srv.Addr().String(),
		nats.Secure(&tls.Config{RootCAs: ses.h.ca, ServerName: "localhost", MinVersion: tls.VersionTLS12}),
		nats.UserInfo(session.UserName(uid, id), ""),
		nats.Token(coordinatortest.Token),
		nats.CustomInboxPrefix(session.InboxPrefix(uid, id)),
		nats.NoReconnect())
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.KeyValue(within(t), bucket)
	require.NoError(t, err)
	return kv
}

// awaitWait waits until a wait of one member in namespace ns watches its keys:
// until the namespace's bucket holds one consumer more than at rest, when the
// coordinator watches the departures and each member, every one connected,
// the session's end.
func (ses *testSession) awaitWait(t *testing.T, ns session.Namespace) {
	t.Helper()
	uid := string(ses.spec.UID)
	bucket, rest := session.SnapshotBucket(uid), len(ses.spec.Members)+1
	if ns == session.NamespaceLibrary {
		bucket, rest = session.LibraryBucket(uid), 0
	}
	ses.awaitConsumers(t, bucket, rest+1)
}

// awaitMostConsumers waits until the session's Snapshot bucket holds its most
// consumers.
func (ses *testSession) awaitMostConsumers(t *testing.T) {
	t.Helper()
	ses.awaitConsumers(t, session.SnapshotBucket(string(ses.spec.UID)), 0)
}

// awaitConsumers waits until one of the session's buckets holds n consumers,
// or its most if n is zero.
func (ses *testSession) awaitConsumers(t *testing.T, bucket string, n int) {
	t.Helper()
	ses.consumersWithin(t, bucket, 10*time.Second, func(held, most int) bool {
		if n == 0 {
			return held >= most
		}
		return held >= n
	})
}

// consumersWithin requires that, within d, ok reports true of how many
// consumers one of the session's buckets holds and how many it may hold, which
// it reads over a plain connection of a member, which holds none.
func (ses *testSession) consumersWithin(t *testing.T, bucket string, d time.Duration, ok func(held, most int) bool) {
	t.Helper()
	ctx := within(t)
	kv := ses.rawBucket(t, 1, bucket)
	require.Eventually(t, func() bool {
		status, err := kv.Status(ctx)
		if err != nil {
			return false
		}
		info := status.(*jetstream.KeyValueBucketStatus).StreamInfo()
		return ok(info.State.Consumers, info.Config.MaxConsumers)
	}, d, 5*time.Millisecond, "the consumers of bucket %s", bucket)
}

// awaitKey waits until kv holds key, and fails the test if it does not within
// the test's bound.
func awaitKey(t *testing.T, kv jetstream.KeyValue, key string) {
	t.Helper()
	ctx, cancel := context.WithCancel(within(t))
	w, err := kv.Watch(ctx, key)
	if err != nil {
		cancel()
		require.NoError(t, err)
	}
	defer func() {
		cancel()
		// The watch's channel closes once its subscription has.
		for range w.Updates() {
		}
	}()
	for {
		select {
		case e := <-w.Updates():
			if e != nil && e.Operation() == jetstream.KeyValuePut {
				return
			}
		case <-ctx.Done():
			t.Fatalf("%s was not written", key)
		}
	}
}

// awaitOutcome waits until the coordinator has seen the session end, and
// returns how: "closed", or "failed" and the reason.
func (ses *testSession) awaitOutcome(t *testing.T) string {
	t.Helper()
	var events []string
	require.Eventually(t, func() bool {
		events = ses.h.observer.of(ses.spec.UID)
		return len(events) > 1
	}, 10*time.Second, 5*time.Millisecond)
	return events[1]
}

// within returns a context the test bounds its waits with.
func within(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// awaitDone waits until a member's session has ended, and fails the test if it
// does not within the test's bound.
func awaitDone(t *testing.T, s coordinator.Session) {
	t.Helper()
	select {
	case <-s.Done():
	case <-within(t).Done():
		t.Fatal("the session did not end")
	}
}

// logLines records every log line.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logger() logr.Logger {
	return funcr.New(func(prefix, args string) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.lines = append(l.lines, prefix+" "+args)
	}, funcr.Options{})
}

func (l *logLines) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.lines)
}

// recorder is an Observer that records what it sees of each attempt.
type recorder struct {
	mu     sync.Mutex
	events map[types.UID][]string
}

func (r *recorder) record(uid types.UID, event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.events == nil {
		r.events = make(map[types.UID][]string)
	}
	r.events[uid] = append(r.events[uid], event)
}

// SessionOpened implements server.Observer.
func (r *recorder) SessionOpened(uid types.UID) { r.record(uid, "opened") }

// SessionClosed implements server.Observer.
func (r *recorder) SessionClosed(uid types.UID) { r.record(uid, "closed") }

// SessionFailed implements server.Observer.
func (r *recorder) SessionFailed(uid types.UID, reason string) { r.record(uid, "failed "+reason) }

// of returns what the recorder saw of one attempt.
func (r *recorder) of(uid types.UID) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events[uid])
}

// proxy forwards TCP connections to the coordinator, so a test can break them
// as a network would, and move them to another coordinator. TLS passes through
// it untouched.
type proxy struct {
	ln       net.Listener
	accepted chan struct{}
	wg       sync.WaitGroup

	mu     sync.Mutex
	target string
	conns  map[net.Conn]struct{}
}

// newProxy starts a proxy to target, and stops it when the test ends.
func newProxy(t *testing.T, target string) *proxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p := &proxy{ln: ln, accepted: make(chan struct{}), target: target, conns: make(map[net.Conn]struct{})}
	go p.accept()
	t.Cleanup(p.stop)
	return p
}

// addr returns the address clients connect to.
func (p *proxy) addr() string { return p.ln.Addr().String() }

// accept forwards every connection until the proxy stops.
func (p *proxy) accept() {
	defer close(p.accepted)
	for {
		in, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		target := p.target
		p.conns[in] = struct{}{}
		p.mu.Unlock()
		p.wg.Add(1)
		go p.forward(in, target)
	}
}

// forward carries one connection to target until either side closes it.
func (p *proxy) forward(in net.Conn, target string) {
	defer p.wg.Done()
	defer p.drop(in)
	out, err := net.Dial("tcp", target)
	if err != nil {
		return
	}
	p.mu.Lock()
	p.conns[out] = struct{}{}
	p.mu.Unlock()
	defer p.drop(out)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(out, in)
		_ = out.Close()
	}()
	_, _ = io.Copy(in, out)
	_ = in.Close()
	<-done
}

// drop closes a connection and forgets it.
func (p *proxy) drop(c net.Conn) {
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
	_ = c.Close()
}

// cut closes every connection the proxy carries. New connections still go
// through it.
func (p *proxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		_ = c.Close()
	}
}

// retarget sends the connections the proxy accepts from now on to target.
func (p *proxy) retarget(target string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.target = target
}

// stop closes the proxy and every connection it carries, and waits until they
// have all ended.
func (p *proxy) stop() {
	_ = p.ln.Close()
	<-p.accepted
	p.cut()
	p.wg.Wait()
}

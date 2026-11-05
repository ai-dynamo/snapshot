// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package sessiontest holds only tests: those of the session client, package
// coordinator, against the real coordinator, package server, and the test that
// ties the client's connection settings to the coordinator's. It is the only
// package that imports both. Each member reaches the coordinator through
// a TCP proxy of its own, which a test cuts to break the member's connection,
// and points at a new coordinator to restart it.
package sessiontest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator"
	"github.com/ai-dynamo/snapshot/coordinator/internal/testclock"
	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"k8s.io/apimachinery/pkg/types"
)

// memberToken is the token the test Authorizer admits.
const memberToken = "member-token"

// harness is a coordinator under test, with an Authorizer the test controls,
// the proxies its members connect through, and a fake clock that times both
// the coordinator's deadlines and grace periods and its members' deadlines.
type harness struct {
	t     *testing.T
	cfg   server.Config
	srv   *server.Server
	ca    *x509.CertPool
	auth  *authorizer
	clock *testclock.Clock
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
	ca, cert := testCertificates(t)
	h := &harness{
		t: t, ca: ca, auth: &authorizer{},
		clock: testclock.New(time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)),
	}
	h.cfg = server.Config{
		Host: "127.0.0.1",
		// Any free port, as nats-server's RANDOM_PORT picks.
		Port:       -1,
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

// session ensures a session with n members for the test t, whose members'
// proxies stop when t ends.
func (h *harness) session(t *testing.T, n int) *testSession {
	t.Helper()
	ses := &testSession{h: h, spec: server.Spec{
		UID:      types.UID(fmt.Sprintf("attempt-%d", time.Now().UnixNano())),
		Deadline: h.clock.Now().Add(time.Hour),
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
		RootCAs:    ses.h.ca,
		ServerName: "localhost",
		Attempt:    string(ses.spec.UID),
		Member:     ses.spec.Members[member].ID,
		Token:      func() (string, error) { return memberToken, nil },
		Clock:      ses.h.clock,
	}
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

// cut breaks every connection of a member, as a broken network would, so its
// client reconnects.
func (ses *testSession) cut(member int) { ses.proxies[member].cut() }

// raw returns the session's Snapshot bucket over a plain NATS connection of
// member, made the way package session defines, which reaches the keys the
// client keeps to itself. The connection closes when the test ends.
func (ses *testSession) raw(t *testing.T, member int) jetstream.KeyValue {
	t.Helper()
	uid, id := string(ses.spec.UID), ses.spec.Members[member].ID
	nc, err := nats.Connect("tls://"+ses.h.srv.Addr().String(),
		nats.Secure(&tls.Config{RootCAs: ses.h.ca, ServerName: "localhost", MinVersion: tls.VersionTLS12}),
		nats.UserInfo(session.UserName(uid, id), ""),
		nats.Token(memberToken),
		nats.CustomInboxPrefix(session.InboxPrefix(uid, id)),
		nats.NoReconnect())
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.KeyValue(within(t), session.SnapshotBucket(uid))
	require.NoError(t, err)
	return kv
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

// authorizer admits memberToken, unless refuse is set, and refuses any other.
type authorizer struct{ refuse atomic.Bool }

// Authorize implements server.Authorizer.
func (a *authorizer) Authorize(_ context.Context, _ types.UID, _, token string) error {
	switch {
	case a.refuse.Load():
		return server.ErrUnavailable
	case token != memberToken:
		return server.ErrUnauthenticated
	}
	return nil
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

// testCertificates returns a CA pool and a serving certificate it signed for
// localhost and 127.0.0.1.
func testCertificates(t *testing.T) (*x509.CertPool, tls.Certificate) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	caCert, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	return pool, tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

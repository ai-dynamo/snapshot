// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/internal/testclock"
	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"k8s.io/apimachinery/pkg/types"
)

// agentToken is the token the test Authorizer admits.
const agentToken = "agent-token"

// harness is a coordinator under test, with a fake clock, an Authorizer the
// test controls, and an Observer that records what it sees.
type harness struct {
	t        *testing.T
	srv      *Server
	clock    *testclock.Clock
	auth     *testAuthorizer
	observer *recorder
	logs     *logLines
	ca       *x509.CertPool
	cfg      Config
}

// newHarness starts a coordinator, marks it ready, and checks that the test
// leaves no goroutine behind.
func newHarness(t *testing.T, modify ...func(*Config)) *harness {
	t.Helper()
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) })
	h := startHarness(t, modify...)
	h.srv.MarkReady()
	return h
}

// startHarness starts a coordinator that is not ready yet.
func startHarness(t *testing.T, modify ...func(*Config)) *harness {
	t.Helper()
	ca, cert := testCertificates(t)
	h := &harness{
		t:        t,
		clock:    testclock.New(time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)),
		auth:     &testAuthorizer{},
		observer: &recorder{},
		logs:     &logLines{},
		ca:       ca,
	}
	h.cfg = Config{
		Host:       "127.0.0.1",
		Port:       natsserver.RANDOM_PORT,
		TLS:        &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		Authorizer: h.auth,
		Clock:      h.clock,
		Observer:   h.observer,
		Log:        h.logs.logger(),
	}
	for _, m := range modify {
		m(&h.cfg)
	}
	srv, err := New(h.cfg)
	require.NoError(t, err)
	h.srv = srv
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	return h
}

// sub returns the harness for a subtest, which reports to t.
func (h *harness) sub(t *testing.T) *harness {
	c := *h
	c.t = t
	return &c
}

// spec returns the spec of a new attempt with n members, named member-0 to
// member-<n-1>, whose deadline is an hour away.
func (h *harness) spec(n int) Spec {
	spec := Spec{UID: types.UID(fmt.Sprintf("uid-%d", time.Now().UnixNano())), Deadline: h.clock.Now().Add(time.Hour)}
	for i := range n {
		spec.Members = append(spec.Members, session.Member{ID: fmt.Sprintf("member-%d", i), Rank: i})
	}
	return spec
}

// ensure ensures a new attempt with n members.
func (h *harness) ensure(n int) Spec {
	h.t.Helper()
	spec := h.spec(n)
	require.NoError(h.t, h.srv.Ensure(within(h.t), spec))
	return spec
}

// dial opens a plain NATS connection as member of spec, the way package session
// defines it: under the member's user name, with token, with the epoch as the
// password unless it is zero, and with the member's own inbox. It never
// reconnects, so a test reconnects a member by dialing again.
func (h *harness) dial(spec Spec, member, token string, epoch uint64, opts ...nats.Option) (*nats.Conn, error) {
	password := ""
	if epoch != 0 {
		password = strconv.FormatUint(epoch, 10)
	}
	uid := string(spec.UID)
	return nats.Connect("tls://"+h.srv.Addr().String(), append([]nats.Option{
		nats.Secure(&tls.Config{RootCAs: h.ca, ServerName: "localhost", MinVersion: tls.VersionTLS12}),
		nats.UserInfo(session.UserName(uid, member), password),
		nats.Token(token),
		nats.CustomInboxPrefix(session.InboxPrefix(uid, member)),
		nats.NoReconnect(),
	}, opts...)...)
}

// member is one member's connection, which the tests drive in place of the
// session client: it has bound both of the attempt's buckets and read Meta,
// and, like the client, it follows the ended key and closes its connection
// once the session has ended.
type member struct {
	id                string
	nc                *nats.Conn
	snapshot, library jetstream.KeyValue
	meta              session.Meta
	done              chan struct{}
	// err is how the session ended, set before done is closed.
	err error
}

// connect connects one member of spec, and closes it when the test ends.
func (h *harness) connect(spec Spec, i int) *member {
	h.t.Helper()
	ctx := within(h.t)
	m := &member{id: spec.Members[i].ID, done: make(chan struct{})}
	var err error
	m.nc, err = h.dial(spec, m.id, agentToken, 0)
	require.NoError(h.t, err)
	h.t.Cleanup(m.nc.Close)
	js, err := jetstream.New(m.nc)
	require.NoError(h.t, err)
	m.snapshot, err = js.KeyValue(ctx, session.SnapshotBucket(string(spec.UID)))
	require.NoError(h.t, err)
	m.library, err = js.KeyValue(ctx, session.LibraryBucket(string(spec.UID)))
	require.NoError(h.t, err)
	entry, err := m.snapshot.Get(ctx, session.KeyMeta)
	require.NoError(h.t, err)
	require.NoError(h.t, json.Unmarshal(entry.Value(), &m.meta))

	// The watch lives until cancel; its channel closes once its subscription
	// has, which closing the connection does too.
	life, cancel := context.WithCancel(context.Background())
	w, err := m.snapshot.Watch(life, session.KeyEnded)
	if err != nil {
		cancel()
		require.NoError(h.t, err)
	}
	stopped := make(chan struct{})
	h.t.Cleanup(func() {
		cancel()
		<-stopped
	})
	go func() {
		defer close(stopped)
		for e := range w.Updates() {
			if e != nil && e.Operation() == jetstream.KeyValuePut {
				m.err = session.EndedError(e.Value())
				close(m.done)
				m.nc.Close()
			}
		}
	}()
	return m
}

// connectAll connects every member of spec.
func (h *harness) connectAll(spec Spec) []*member {
	members := make([]*member, len(spec.Members))
	for i := range spec.Members {
		members[i] = h.connect(spec, i)
	}
	return members
}

// Err returns nil until the member has seen its session end, and then how.
func (m *member) Err() error {
	select {
	case <-m.done:
		return m.err
	default:
		return nil
	}
}

// bucket returns the bucket that holds a namespace.
func (m *member) bucket(ns session.Namespace) jetstream.KeyValue {
	if ns == session.NamespaceLibrary {
		return m.library
	}
	return m.snapshot
}

// insert creates key in a namespace.
func (m *member) insert(ctx context.Context, ns session.Namespace, key string, value []byte) error {
	_, err := m.bucket(ns).Create(ctx, string(ns)+"."+key, value)
	return err
}

// arrive records the member's arrival at a barrier.
func (m *member) arrive(ctx context.Context, barrier string) error {
	_, err := m.snapshot.Put(ctx, session.BarrierKey(barrier, m.id), nil)
	return err
}

// leave records the member's departure. Like the client's Leave, it succeeds
// if the session closed: the last departure closes it, and the member's own
// watch may see that, and close the connection, before the write's reply
// arrives.
func (m *member) leave(ctx context.Context) error {
	_, err := m.snapshot.Put(ctx, session.LeftKey(m.id), nil)
	if err != nil && errors.Is(m.Err(), session.ErrClosed) {
		return nil
	}
	return err
}

// fail ends the session with reason, unless it has ended. Like the client's
// Fail, it succeeds if the session ended with reason: the member's own watch
// may see the ended key, and close the connection, before the write's reply
// arrives.
func (m *member) fail(ctx context.Context, reason string) error {
	_, err := m.snapshot.Create(ctx, session.KeyEnded, []byte(reason))
	if err != nil && errors.Is(m.Err(), &session.AbortError{Reason: reason}) {
		return nil
	}
	return err
}

// raw opens a member's connection that reports every error the server sends
// it, such as a permission violation, and closes it when the test ends.
func (h *harness) raw(spec Spec, member int) (*nats.Conn, <-chan error) {
	h.t.Helper()
	violations := make(chan error, 64)
	nc, err := h.dial(spec, spec.Members[member].ID, agentToken, 0,
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			select {
			case violations <- err:
			default:
			}
		}))
	require.NoError(h.t, err)
	h.t.Cleanup(nc.Close)
	return nc, violations
}

// kick closes every connection of a member from the server's side, as a broken
// network would.
func (h *harness) kick(spec Spec, member int) {
	h.t.Helper()
	connz, err := h.srv.ns.Connz(&natsserver.ConnzOptions{
		User: session.UserName(string(spec.UID), spec.Members[member].ID),
	})
	require.NoError(h.t, err)
	require.NotEmpty(h.t, connz.Conns, "the member has no connection to kick")
	for _, c := range connz.Conns {
		require.NoError(h.t, h.srv.ns.DisconnectClientByID(c.Cid))
	}
}

// ended returns the ended key of an attempt, and false if it is absent.
func (h *harness) ended(spec Spec) (string, bool) {
	h.t.Helper()
	a := h.srv.lookup(spec.UID)
	require.NotNil(h.t, a)
	entry, err := a.snapshot.Get(within(h.t), session.KeyEnded)
	if err != nil {
		return "", false
	}
	return string(entry.Value()), true
}

// awaitEnd waits until a member has seen its session end, and returns how.
func awaitEnd(t *testing.T, m *member) error {
	t.Helper()
	select {
	case <-m.done:
		return m.err
	case <-time.After(10 * time.Second):
		t.Fatal("the session did not end")
		return nil
	}
}

// watchUntil watches key of kv, which may hold wildcards, the way a client's
// waits do, until done reports true of the keys seen so far, each true while
// present. It fails the test if that takes too long.
func watchUntil(t *testing.T, kv jetstream.KeyValue, key string, done func(present map[string]bool) bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	w, err := kv.Watch(ctx, key)
	if err != nil {
		cancel()
		require.NoError(t, err)
	}
	defer func() {
		cancel()
		for range w.Updates() {
		}
	}()
	present := make(map[string]bool)
	for {
		select {
		case e, ok := <-w.Updates():
			require.True(t, ok, "the watch of %s stopped", key)
			if e != nil {
				present[e.Key()] = e.Operation() == jetstream.KeyValuePut
			}
			if done(present) {
				return
			}
		case <-ctx.Done():
			t.Fatalf("the watch of %s did not see what it waited for", key)
		}
	}
}

// within returns a context the test bounds its waits with.
func within(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// testAuthorizer admits agentToken, and returns err for any other, or for
// every token while err is set.
type testAuthorizer struct {
	mu    sync.Mutex
	err   error
	calls atomic.Int64
}

// Authorize implements Authorizer.
func (a *testAuthorizer) Authorize(_ context.Context, _ types.UID, _, token string) error {
	a.calls.Add(1)
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.err != nil:
		return a.err
	case token != agentToken:
		return ErrUnauthenticated
	}
	return nil
}

// fail makes the Authorizer return err for every token, or admit agentToken
// again when err is nil.
func (a *testAuthorizer) fail(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.err = err
}

// recorder is an Observer that records what it sees.
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) record(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

// SessionOpened implements Observer.
func (r *recorder) SessionOpened(uid types.UID) { r.record("opened " + string(uid)) }

// SessionClosed implements Observer.
func (r *recorder) SessionClosed(uid types.UID) { r.record("closed " + string(uid)) }

// SessionFailed implements Observer.
func (r *recorder) SessionFailed(uid types.UID, reason string) {
	r.record("failed " + string(uid) + " " + reason)
}

// of returns what the recorder saw of one attempt.
func (r *recorder) of(uid types.UID) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var events []string
	for _, e := range r.events {
		var kind, id string
		_, _ = fmt.Sscanf(e, "%s %s", &kind, &id)
		if id == string(uid) {
			events = append(events, e)
		}
	}
	return events
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
	}, funcr.Options{Verbosity: 2})
}

func (l *logLines) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
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

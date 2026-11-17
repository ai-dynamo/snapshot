// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/session"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"k8s.io/apimachinery/pkg/types"
)

// pending is how long a test watches a call to check that it has not returned.
const pending = 300 * time.Millisecond

// redialWait spaces connectLater's attempts.
const redialWait = 50 * time.Millisecond

// connectLater keeps dialing as a member in the background, as the client does
// while the coordinator refuses it, until the coordinator admits it. It
// reports the last refusal if the test's wait ends first.
func connectLater(h *harness, spec Spec, member int) <-chan error {
	ctx := within(h.t)
	out := make(chan error, 1)
	go func() {
		for {
			nc, err := h.dial(spec, spec.Members[member].ID, agentToken, 0)
			if err == nil {
				nc.Close()
				out <- nil
				return
			}
			select {
			case <-ctx.Done():
				out <- err
				return
			case <-time.After(redialWait):
			}
		}
	}()
	return out
}

// requireNoGrace checks that no member of spec has a grace period running.
func (h *harness) requireNoGrace(spec Spec) {
	h.t.Helper()
	time.Sleep(pending)
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	assert.Empty(h.t, h.srv.attempts[spec.UID].grace)
}

// requirePending fails the test if the call has returned.
func requirePending(t *testing.T, call <-chan error) {
	t.Helper()
	select {
	case err := <-call:
		t.Fatalf("the call returned early: %v", err)
	case <-time.After(pending):
	}
}

// TestAuthentication covers who the coordinator admits.
func TestAuthentication(t *testing.T) {
	h := newHarness(t)

	t.Run("admits a member the Authorizer admits", func(t *testing.T) {
		h := h.sub(t)
		m := h.connect(h.ensure(2), 1)
		assert.Equal(t, "member-1", m.meta.Members[1].ID)
	})

	for _, refusal := range []error{ErrUnauthenticated, ErrPermissionDenied} {
		t.Run("refuses a member the Authorizer refuses with "+refusal.Error(), func(t *testing.T) {
			h := h.sub(t)
			spec := h.ensure(1)
			h.auth.fail(refusal)
			defer h.auth.fail(nil)
			_, err := h.dial(spec, spec.Members[0].ID, agentToken, 0)
			require.ErrorIs(t, err, nats.ErrAuthorization)
		})
	}

	for _, transient := range []error{ErrUnavailable, session.ErrNotFound, errors.New("outside the contract")} {
		t.Run("admits a member once the Authorizer recovers from "+transient.Error(), func(t *testing.T) {
			h := h.sub(t)
			spec := h.ensure(1)
			h.auth.fail(transient)
			defer h.auth.fail(nil)
			connected := connectLater(h, spec, 0)
			requirePending(t, connected)
			h.auth.fail(nil)
			require.NoError(t, <-connected)
		})
	}

	t.Run("refuses a token the Authorizer does not know", func(t *testing.T) {
		h := h.sub(t)
		spec := h.ensure(1)
		_, err := h.dial(spec, spec.Members[0].ID, "stolen", 0)
		require.ErrorIs(t, err, nats.ErrAuthorization)
	})

	t.Run("logs the Authorizer's refusals at Info only for a member of a session", func(t *testing.T) {
		h := h.sub(t)
		held, unheld := h.ensure(1), h.spec(1)
		for _, spec := range []Spec{held, unheld} {
			_, err := h.dial(spec, spec.Members[0].ID, "stolen", 1)
			require.ErrorIs(t, err, nats.ErrAuthorization)
		}
		const refused = `"msg"="Refused a member the Authorizer refused"`
		h.awaitLog(held, `"level"=0 `+refused, time.Second)
		h.awaitLog(unheld, `"level"=1 `+refused, time.Second)
	})

	t.Run("refuses a client no session could admit without asking the Authorizer", func(t *testing.T) {
		h := h.sub(t)
		spec := h.ensure(1)
		asked := h.auth.calls.Load()
		_, err := h.dial(spec, "stranger", agentToken, 1)
		require.ErrorIs(t, err, nats.ErrAuthorization, "a client that is not a member")
		_, err = h.dial(h.spec(1), "member-0", agentToken, 0)
		require.ErrorIs(t, err, nats.ErrAuthorization, "a client joining an attempt with no session")
		assert.Equal(t, asked, h.auth.calls.Load(), "the Authorizer must not be asked")
	})

	t.Run("refuses a client that is not a member", func(t *testing.T) {
		h := h.sub(t)
		_, err := h.dial(h.ensure(1), "stranger", agentToken, 0)
		require.ErrorIs(t, err, nats.ErrAuthorization)
	})

	t.Run("admits a member reconnecting to no session, which finds no bucket", func(t *testing.T) {
		h := h.sub(t)
		spec := h.spec(1)
		nc, err := h.dial(spec, spec.Members[0].ID, agentToken, 1)
		require.NoError(t, err)
		defer nc.Close()
		js, err := jetstream.New(nc)
		require.NoError(t, err)
		_, err = js.KeyValue(within(t), session.ControlBucket(string(spec.UID)))
		require.ErrorIs(t, err, jetstream.ErrBucketNotFound)
	})

	t.Run("tells the Authorizer whether the coordinator holds the session", func(t *testing.T) {
		h := h.sub(t)
		spec := h.ensure(1)
		h.connect(spec, 0)
		assert.True(t, h.auth.held.Load(), "a member of a session the coordinator holds")
		unheld := h.spec(1)
		nc, err := h.dial(unheld, unheld.Members[0].ID, agentToken, 1)
		require.NoError(t, err)
		nc.Close()
		assert.False(t, h.auth.held.Load(), "a member reconnecting to a session the coordinator lost")
	})

	t.Run("admits a member of a removed session, whatever its placement, so it learns the session is gone",
		func(t *testing.T) {
			h := h.sub(t)
			spec := h.ensure(1)
			m := h.connect(spec, 0)
			require.NoError(t, h.srv.Remove(within(t), spec.UID))
			require.ErrorIs(t, awaitEnd(t, m), &session.AbortError{Reason: session.ReasonCancelled})
			// The group resource is gone, so the Authorizer does not place the
			// member. A held session still needs its placement, which a lagging
			// cache may not show yet.
			h.auth.unplace(true)
			defer h.auth.unplace(false)
			nc, err := h.dial(spec, spec.Members[0].ID, agentToken, m.meta.Epoch)
			require.NoError(t, err)
			defer nc.Close()
			js, err := jetstream.New(nc)
			require.NoError(t, err)
			_, err = js.KeyValue(within(t), session.ControlBucket(string(spec.UID)))
			require.ErrorIs(t, err, jetstream.ErrBucketNotFound, "the member must reach the coordinator to learn that")
		})

	t.Run("refuses a member of a held session while its placement is unknown", func(t *testing.T) {
		h := h.sub(t)
		spec := h.ensure(1)
		h.auth.unplace(true)
		defer h.auth.unplace(false)
		connected := connectLater(h, spec, 0)
		requirePending(t, connected)
		h.auth.unplace(false)
		require.NoError(t, <-connected)
	})

	t.Run("a refused client starts no grace period for the member it names", func(t *testing.T) {
		h := h.sub(t)
		spec := h.ensure(1)
		h.connect(spec, 0)
		_, err := h.dial(spec, spec.Members[0].ID, "stolen", 0)
		require.ErrorIs(t, err, nats.ErrAuthorization)
		h.requireNoGrace(spec)
	})

	t.Run("admits a member once its attempt has a session", func(t *testing.T) {
		h := h.sub(t)
		spec := h.spec(1)
		connected := connectLater(h, spec, 0)
		requirePending(t, connected)
		require.NoError(t, h.srv.Ensure(within(t), spec))
		require.NoError(t, <-connected)
	})
}

func TestSessionAppearsDuringAuthorization(t *testing.T) {
	var h *harness
	var spec Spec
	lookups := make(chan bool, 2)
	auth := authorizeFunc(func(ctx context.Context, _ types.UID, _, _ string, held bool) error {
		lookups <- held
		if held {
			return ErrPermissionDenied
		}
		return h.srv.Ensure(ctx, spec)
	})
	h = newHarness(t, func(cfg *Config) { cfg.Authorizer = auth })
	spec = h.spec(1)
	nc, err := h.dial(spec, spec.Members[0].ID, agentToken, 1)
	if nc != nil {
		defer nc.Close()
	}
	require.ErrorIs(t, err, nats.ErrAuthorization, "placement was not checked for the session that appeared")
	assert.False(t, <-lookups)
	nc, err = h.dial(spec, spec.Members[0].ID, agentToken, 1)
	if nc != nil {
		defer nc.Close()
	}
	require.ErrorIs(t, err, nats.ErrAuthorization, "a retry must check placement")
	assert.True(t, <-lookups)
}

type authorizeFunc func(context.Context, types.UID, string, string, bool) error

func (f authorizeFunc) Authorize(ctx context.Context, uid types.UID, member, token string, held bool) error {
	return f(ctx, uid, member, token, held)
}

// TestReadiness covers the gate: until MarkReady, every client is refused, and
// the Authorizer is not even asked, and Ready reports it, until Shutdown.
func TestReadiness(t *testing.T) {
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) })
	h := startHarness(t)
	spec := h.ensure(1)
	connected := connectLater(h, spec, 0)
	requirePending(t, connected)
	assert.Zero(t, h.auth.calls.Load(), "the Authorizer must not be asked before the coordinator is ready")
	assert.False(t, h.srv.Ready())
	h.srv.MarkReady()
	require.NoError(t, <-connected)
	assert.True(t, h.srv.Ready())
	require.NoError(t, h.srv.Shutdown(within(t)))
	assert.False(t, h.srv.Ready(), "a coordinator that shut down is not ready")
}

// TestInternalToken checks that the coordinator's own token admits nothing
// over the network: only an in-process pipe may present it.
func TestInternalToken(t *testing.T) {
	h := newHarness(t)
	for _, tt := range []struct {
		name   string
		user   string
		token  string
		remote net.Addr
		want   bool
	}{
		{"the coordinator's connection", coordinatorUser, h.srv.token, pipeAddr{}, true},
		{"the events connection", eventsUser, h.srv.token, pipeAddr{}, true},
		{"the token over TCP", coordinatorUser, h.srv.token, &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1)}, false},
		{"another token", coordinatorUser, "guess", pipeAddr{}, false},
		{"another user", "admin", h.srv.token, pipeAddr{}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := &fakeClient{opts: natsserver.ClientOpts{Username: tt.user, Token: tt.token}, remote: tt.remote}
			_, ok := h.srv.internal(c, &c.opts)
			assert.Equal(t, tt.want, ok)
		})
	}
}

// TestEpochFence covers a member that reconnects with the epoch of a session
// the coordinator no longer holds: the coordinator fails the session it holds
// now before admitting the member, so the member never writes to it, and every
// other member learns the end, those that had not connected yet included.
func TestEpochFence(t *testing.T) {
	h := newHarness(t)
	ctx := within(t)
	spec := h.ensure(3)
	old := h.srv.lookup(spec.UID).epoch
	// The host never recreates an attempt it removed; a test may, to
	// give it another epoch, as a restart would.
	require.NoError(t, h.srv.Remove(ctx, spec.UID))
	spec2 := spec
	require.NoError(t, h.srv.Ensure(ctx, spec2))
	require.NotEqual(t, old, h.srv.lookup(spec2.UID).epoch)
	fresh := h.connect(spec2, 1)
	require.NoError(t, fresh.Err())

	// A member presenting the old epoch is admitted only after the
	// coordinator has failed the session.
	nc, err := h.dial(spec, spec.Members[0].ID, agentToken, old)
	require.NoError(t, err)
	defer nc.Close()
	reason, ok := h.ended(spec2)
	require.True(t, ok, "the session must have failed by the time the member is admitted")
	assert.Equal(t, session.ReasonCoordinatorLost, reason)
	require.ErrorIs(t, awaitEnd(t, fresh), session.ErrCoordinatorLost)
	late := h.connect(spec2, 2)
	require.ErrorIs(t, awaitEnd(t, late), session.ErrCoordinatorLost, "a member that had not connected yet")
}

// TestReadOnlyWithoutSession covers a member that presents an epoch while the
// coordinator holds no session for its attempt: the coordinator admits it
// read-only. Once the host creates the session, the member can still bind its
// buckets, read meta, and watch keys. So it finds the other epoch, and learns
// that its session is gone. None of its writes reaches the session.
func TestReadOnlyWithoutSession(t *testing.T) {
	h := newHarness(t)
	ctx := within(t)
	spec := h.spec(2)
	uid, id := string(spec.UID), spec.Members[0].ID
	// lost is the epoch of the session the member joined, which the
	// coordinator no longer holds.
	const lost = 1
	violations := make(chan error, 64)
	nc, err := h.dial(spec, id, agentToken, lost,
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			select {
			case violations <- err:
			default:
			}
		}))
	require.NoError(t, err)
	defer nc.Close()
	require.NoError(t, h.srv.Ensure(ctx, spec))

	// What the client's verification does: bind the control bucket and read
	// meta. What its waits do: watch keys.
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	control, err := js.KeyValue(ctx, session.ControlBucket(uid))
	require.NoError(t, err)
	entry, err := control.Get(ctx, session.KeyMeta)
	require.NoError(t, err)
	var meta session.Meta
	require.NoError(t, json.Unmarshal(entry.Value(), &meta))
	require.NotEqual(t, uint64(lost), meta.Epoch, "the member must find another epoch")
	watchUntil(t, control, session.KeyEnded, func(map[string]bool) bool { return true })

	for name, subject := range map[string]string{
		"its arrival":     session.Subject(session.ControlBucket(uid), session.BarrierKey("admitted", id)),
		"its departure":   session.Subject(session.ControlBucket(uid), session.LeftKey(id)),
		"the end":         session.Subject(session.ControlBucket(uid), session.KeyEnded),
		"an agent's key":  session.Subject(session.SnapshotBucket(uid), "snapshot.k"),
		"a library's key": session.Subject(session.LibraryBucket(uid), "library.k"),
	} {
		t.Run(name, func(t *testing.T) {
			_, _ = nc.Request(subject, []byte(session.ReasonRefused), pending)
			requireViolation(t, violations, `Publish to "`+subject+`"`)
		})
	}
	// Meta is the only record the session holds.
	for _, bucket := range session.Buckets(uid) {
		stream, err := h.srv.js.Stream(ctx, "KV_"+bucket)
		require.NoError(t, err)
		info, err := stream.Info(ctx)
		require.NoError(t, err)
		want := uint64(0)
		if bucket == session.ControlBucket(uid) {
			want = 1
		}
		assert.Equal(t, want, info.State.Msgs, "the records of bucket %s", bucket)
	}
}

// pipeAddr is the address of an in-process connection.
type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

// fakeClient is a client as the authentication hook sees it.
type fakeClient struct {
	opts   natsserver.ClientOpts
	remote net.Addr
}

func (c *fakeClient) GetOpts() *natsserver.ClientOpts             { return &c.opts }
func (c *fakeClient) GetTLSConnectionState() *tls.ConnectionState { return nil }
func (c *fakeClient) RegisterUser(*natsserver.User)               {}
func (c *fakeClient) RemoteAddress() net.Addr                     { return c.remote }
func (c *fakeClient) GetNonce() []byte                            { return nil }
func (c *fakeClient) Kind() int                                   { return natsserver.CLIENT }
func (c *fakeClient) GetID() uint64                               { return 1 }

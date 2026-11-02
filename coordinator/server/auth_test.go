// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/tls"
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

	for _, transient := range []error{ErrUnavailable, ErrNotFound, errors.New("outside the contract")} {
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
		_, err = js.KeyValue(within(t), session.SnapshotBucket(string(spec.UID)))
		require.ErrorIs(t, err, jetstream.ErrBucketNotFound)
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

// TestReadiness covers the gate: until MarkReady, every client is refused, and
// the Authorizer is not even asked.
func TestReadiness(t *testing.T) {
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) })
	h := startHarness(t)
	spec := h.ensure(1)
	connected := connectLater(h, spec, 0)
	requirePending(t, connected)
	assert.Zero(t, h.auth.calls.Load(), "the Authorizer must not be asked before the coordinator is ready")
	h.srv.MarkReady()
	require.NoError(t, <-connected)
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
// now before admitting the member, so the member never writes to it.
func TestEpochFence(t *testing.T) {
	h := newHarness(t)
	ctx := within(t)
	spec := h.ensure(2)
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

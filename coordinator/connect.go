// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/go-logr/logr"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"k8s.io/utils/clock"
)

// Config is what a member needs to connect to its session.
type Config struct {
	// URLs are the coordinator's client URLs, such as
	// tls://snapshot-coordinator.snapshot-system.svc:4222. The client
	// connects to any of them, and reconnects to any of them.
	URLs []string

	// RootCAs verifies the coordinator's serving certificate. The connection
	// always uses TLS.
	RootCAs *x509.CertPool

	// ServerName is the name the coordinator's certificate must carry. It
	// defaults to each URL's host.
	ServerName string

	// Attempt is the UID of the group resource, a PodGroupSnapshot or a
	// PodGroupRestore, which identifies the attempt.
	Attempt string

	// Member is the member the connection acts for.
	Member string

	// Token returns the agent's audience-scoped, projected ServiceAccount
	// token. It is called each time the client connects, so a rotated token
	// is picked up when the client reconnects.
	Token func() (string, error)

	// Log receives the connection's life: connects, reconnects, and the
	// coordinator being lost. It is never given a key, a value, a barrier
	// name, or a token. The zero Logger discards everything.
	Log logr.Logger

	// Clock times the session's deadline. It defaults to the real clock.
	Clock clock.Clock
}

// validate reports session.ErrInvalid for a Config that cannot connect.
func (c Config) validate() error {
	switch {
	case len(c.URLs) == 0:
		return fmt.Errorf("no coordinator URL: %w", session.ErrInvalid)
	case c.RootCAs == nil:
		return fmt.Errorf("no CA pool for the coordinator's certificate: %w", session.ErrInvalid)
	case c.Token == nil:
		return fmt.Errorf("no token source: %w", session.ErrInvalid)
	}
	if err := session.ValidateAttempt(c.Attempt); err != nil {
		return fmt.Errorf("attempt: %w", err)
	}
	if err := session.ValidateName(c.Member); err != nil {
		return fmt.Errorf("member: %w", err)
	}
	return nil
}

// The timing of a connection.
const (
	// reconnectWait and reconnectJitter space a client's reconnect attempts:
	// short, so a broken connection is restored well within the grace period.
	reconnectWait   = 250 * time.Millisecond
	reconnectJitter = 100 * time.Millisecond

	// attemptTimeout bounds one round trip of an operation before the session
	// sends it again. Only metadata crosses a session, so it is generous.
	attemptTimeout = 3 * time.Second

	// retryWait spaces the resends of an operation that failed transiently.
	retryWait = 100 * time.Millisecond
)

// Connect connects to the coordinator as cfg.Member of the attempt cfg.Attempt,
// and reads the session's Meta. An agent connects once for each member it
// hosts. It waits while the coordinator refuses the
// connection, which it does until it holds the session and has checked the
// token, and reports session.ErrForbidden, together with ctx's error, if ctx
// is done before it is admitted. Since the coordinator admits a joining member
// only once the session exists, it reports session.ErrNotFound only if the
// session is removed while the member joins.
func Connect(ctx context.Context, cfg Config) (Session, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.RealClock{}
	}
	s := &client{
		attempt:    cfg.Attempt,
		member:     cfg.Member,
		log:        cfg.Log.WithValues("attempt", cfg.Attempt, "member", cfg.Member),
		clock:      cfg.Clock,
		connected:  make(chan struct{}),
		verify:     make(chan struct{}, 1),
		closing:    make(chan struct{}),
		connClosed: make(chan struct{}),
		done:       make(chan struct{}),
		wake:       make(chan struct{}),
	}
	nc, err := nats.Connect(strings.Join(cfg.URLs, ","), s.options(cfg)...)
	if err != nil {
		return nil, fmt.Errorf("connect to the coordinator: %w", mapError(err))
	}
	s.nc = nc
	if err := s.join(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	s.log.Info("Joined the session")
	return s, nil
}

// options returns the connection's options, which carry the session's identity
// and its connect, reconnection, and keepalive settings.
func (s *client) options(cfg Config) []nats.Option {
	return []nats.Option{
		nats.Secure(&tls.Config{RootCAs: cfg.RootCAs, ServerName: cfg.ServerName, MinVersion: tls.VersionTLS12}),
		nats.UserInfoHandler(func() (string, string) {
			if epoch := s.epoch.Load(); epoch != 0 {
				return session.UserName(s.attempt, s.member), strconv.FormatUint(epoch, 10)
			}
			return session.UserName(s.attempt, s.member), ""
		}),
		nats.TokenHandler(func() string {
			token, err := cfg.Token()
			if err != nil {
				s.log.Error(err, "Reading the ServiceAccount token failed; the coordinator will refuse this attempt")
			}
			return token
		}),
		nats.CustomInboxPrefix(session.InboxPrefix(s.attempt, s.member)),
		// The coordinator refuses a client until it holds the session, so a
		// refusal is worth retrying, and so is a first connection that fails.
		nats.RetryOnFailedConnect(true),
		nats.IgnoreAuthErrorAbort(),
		nats.MaxReconnects(-1),
		// Nothing is buffered while reconnecting: a publish then fails at
		// once, and the session sends it again once the connection is back,
		// so a write never reports a failure and lands later.
		nats.ReconnectBufSize(-1),
		nats.ReconnectWait(reconnectWait),
		nats.ReconnectJitter(reconnectJitter, reconnectJitter),
		nats.Timeout(ClientConnectTimeout),
		nats.PingInterval(ClientPingInterval),
		nats.MaxPingsOutstanding(ClientMaxPingsOut),
		nats.ConnectHandler(func(*nats.Conn) { s.connect.Do(func() { close(s.connected) }) }),
		nats.ReconnectHandler(func(*nats.Conn) {
			s.log.Info("Reconnected to the coordinator")
			s.wakeVerifier()
		}),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			s.inflight.interrupt()
			if err != nil {
				s.log.Info("Lost the connection to the coordinator; reconnecting", "error", err.Error())
			}
		}),
		nats.ClosedHandler(func(*nats.Conn) { close(s.connClosed) }),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			// A refused publish names its subject, which can hold a key, so its
			// text is never logged.
			if errors.Is(err, nats.ErrPermissionViolation) {
				s.log.Info("The coordinator refused a publish or a subscription this connection is not permitted")
				return
			}
			s.log.Info("The coordinator reported an error", "error", err.Error())
		}),
	}
}

// join waits until the coordinator admits the connection, binds both buckets,
// reads Meta, and starts verifying the session when asked, following its end,
// and timing its deadline.
func (s *client) join(ctx context.Context) error {
	select {
	case <-s.connected:
	case <-ctx.Done():
		if errors.Is(s.nc.LastError(), nats.ErrAuthorization) {
			return fmt.Errorf("the coordinator refused the connection (%w) until: %w", session.ErrForbidden, ctx.Err())
		}
		return fmt.Errorf("connect to the coordinator: %w", ctx.Err())
	}
	js, err := jetstream.New(s.nc)
	if err != nil {
		return mapError(err)
	}
	if s.snapshot, err = s.bind(ctx, js, session.SnapshotBucket(s.attempt)); err != nil {
		return err
	}
	if s.library, err = s.bind(ctx, js, session.LibraryBucket(s.attempt)); err != nil {
		return err
	}
	s.mu.Lock()
	target := s.target()
	s.mu.Unlock()
	if err := s.do(ctx, func(ctx context.Context) error {
		s.meta, err = readMeta(ctx, s.snapshot)
		return err
	}); err != nil {
		return err
	}
	s.ranks = make(map[string]int, len(s.meta.Members))
	for _, m := range s.meta.Members {
		s.ranks[m.ID] = m.Rank
	}
	if _, ok := s.ranks[s.member]; !ok {
		return fmt.Errorf("the session has no member %q: %w", s.member, session.ErrForbidden)
	}
	s.epoch.Store(s.meta.Epoch)
	// Reading Meta verified every reconnect before it.
	s.settled(target)
	s.joined = true

	s.wg.Add(3)
	go s.verifier()
	go s.followEnd()
	go s.expire()
	return nil
}

// bind binds one of the attempt's buckets.
func (s *client) bind(ctx context.Context, js jetstream.JetStream, bucket string) (jetstream.KeyValue, error) {
	var kv jetstream.KeyValue
	err := s.do(ctx, func(ctx context.Context) error {
		var err error
		kv, err = js.KeyValue(ctx, bucket)
		return err
	})
	return kv, err
}

// readMeta reads and decodes the meta key.
func readMeta(ctx context.Context, kv jetstream.KeyValue) (session.Meta, error) {
	entry, err := kv.Get(ctx, session.KeyMeta)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return session.Meta{}, fmt.Errorf("the session has no meta: %w", session.ErrNotFound)
	}
	if err != nil {
		return session.Meta{}, err
	}
	var meta session.Meta
	if err := json.Unmarshal(entry.Value(), &meta); err != nil {
		return session.Meta{}, fmt.Errorf("decode the session's meta: %w", err)
	}
	return meta, nil
}

// Rank implements Session.
func (s *client) Rank() int { return s.ranks[s.member] }

// Meta implements Session.
func (s *client) Meta() session.Meta {
	meta := s.meta
	meta.Members = slices.Clone(s.meta.Members)
	return meta
}

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
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ai-dynamo/snapshot/agent/pkg/session"
	"github.com/go-logr/logr"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
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

	// Attempt is the UID of the set resource, which identifies the attempt.
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
	if err := session.ValidateName(c.Attempt); err != nil {
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
// and reads the session's Meta. It waits while the coordinator refuses the
// connection, which it does until it holds the session and has checked the
// token, and reports session.ErrForbidden, together with ctx's error, if ctx
// is done before it is admitted. Since the coordinator admits a joining member
// only once the session exists, it reports session.ErrNotFound only if the
// session is removed while the member joins.
func Connect(ctx context.Context, cfg Config) (Session, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	s := &client{
		attempt:     cfg.Attempt,
		member:      cfg.Member,
		log:         cfg.Log.WithValues("attempt", cfg.Attempt, "member", cfg.Member),
		connected:   make(chan struct{}),
		reconnected: make(chan struct{}, 1),
		closing:     make(chan struct{}),
		done:        make(chan struct{}),
		wake:        make(chan struct{}),
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
// and its reconnection and keepalive settings.
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
		nats.ReconnectWait(reconnectWait),
		nats.ReconnectJitter(reconnectJitter, reconnectJitter),
		nats.PingInterval(ClientPingInterval),
		nats.MaxPingsOutstanding(ClientMaxPingsOut),
		nats.ConnectHandler(func(*nats.Conn) { s.connect.Do(func() { close(s.connected) }) }),
		nats.ReconnectHandler(func(*nats.Conn) {
			s.log.Info("Reconnected to the coordinator")
			select {
			case s.reconnected <- struct{}{}:
			default:
			}
		}),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			s.inflight.interrupt()
			if err != nil {
				s.log.Info("Lost the connection to the coordinator; reconnecting", "error", err.Error())
			}
		}),
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
// reads Meta, and starts following the session's end and the connection's
// reconnects.
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
	reconnects := s.reconnects()
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
	s.verified = reconnects

	ended, err := s.watch(ctx, s.snapshot, []string{session.KeyEnded})
	if err != nil {
		return err
	}
	s.wg.Add(2)
	go s.followEnd(ended)
	go s.followReconnects()
	return nil
}

// bind binds one of the attempt's buckets.
func (s *client) bind(ctx context.Context, js jetstream.JetStream, bucket string) (jetstream.KeyValue, error) {
	var kv jetstream.KeyValue
	err := s.do(ctx, func(ctx context.Context) error {
		var err error
		kv, err = js.KeyValue(ctx, bucket)
		if errors.Is(err, jetstream.ErrBucketNotFound) {
			return fmt.Errorf("attempt %s has no session: %w", s.attempt, session.ErrNotFound)
		}
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

// followReconnects checks the epoch after every reconnect.
func (s *client) followReconnects() {
	defer s.wg.Done()
	for {
		select {
		case <-s.reconnected:
			s.checkEpoch()
		case <-s.closing:
			return
		case <-s.done:
			return
		}
	}
}

// checkEpoch reads Meta again after a reconnect. The same epoch means the
// coordinator still holds the session the member joined. Another epoch, or no
// Meta, means it recreated the session after losing it, and no bucket means it
// lost the session for good: either way, the session ends with
// session.ErrCoordinatorLost, after failing a recreated session with that
// reason.
func (s *client) checkEpoch() {
	for {
		reconnects := s.reconnects()
		ctx, cancel := context.WithTimeout(context.Background(), attemptTimeout)
		_, err := s.snapshot.Status(ctx)
		var meta session.Meta
		if err == nil {
			meta, err = readMeta(ctx, s.snapshot)
		}
		cancel()
		switch {
		case err == nil && meta.Epoch == s.meta.Epoch:
			s.settled(reconnects)
			return
		case err == nil, errors.Is(err, session.ErrNotFound):
			s.lose(true)
			return
		case errors.Is(err, jetstream.ErrStreamNotFound), errors.Is(err, jetstream.ErrBucketNotFound):
			s.lose(false)
			return
		}
		select {
		case <-time.After(retryWait):
		case <-s.closing:
			return
		case <-s.done:
			return
		}
	}
}

// lose ends a session the coordinator lost. If the coordinator recreated it,
// the session first fails the recreated one, so every member learns at once;
// creating ended fails only if something ended it already, which serves as
// well.
func (s *client) lose(recreated bool) {
	if recreated {
		ctx, cancel := context.WithTimeout(context.Background(), attemptTimeout)
		_, _ = s.snapshot.Create(ctx, session.KeyEnded, []byte(session.ReasonCoordinatorLost))
		cancel()
	}
	s.log.Info("The coordinator lost the session")
	s.finish(session.ErrCoordinatorLost)
}

// settled records that the epoch was found unchanged on the connection made by
// reconnect number reconnects, and wakes every caller waiting for it.
func (s *client) settled(reconnects uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if reconnects > s.verified {
		s.verified = reconnects
		close(s.wake)
		s.wake = make(chan struct{})
	}
}

// settle returns once the epoch has been checked on the current connection. A
// wait calls it before returning what it read, so it never reports a value or
// a release it read from a session the coordinator recreated: after a
// coordinator restart, a watch resumes on the recreated bucket, and it may
// deliver an entry before the epoch check has ended the session.
func (s *client) settle(ctx context.Context) error {
	for {
		s.mu.Lock()
		ok, wake := s.verified >= s.reconnects(), s.wake
		s.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-wake:
		case <-s.done:
			return s.Err()
		case <-s.closing:
			return net.ErrClosed
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// reconnects returns how many times the connection has reconnected.
func (s *client) reconnects() uint64 { return s.nc.Stats().Reconnects }

// Meta implements Session.
func (s *client) Meta() session.Meta {
	meta := s.meta
	meta.Members = slices.Clone(s.meta.Members)
	return meta
}

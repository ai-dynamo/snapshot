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
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/go-logr/logr"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"golang.org/x/sync/semaphore"
	"k8s.io/utils/clock"
)

// Config is what a member needs to connect to its session.
type Config struct {
	// URLs are the coordinator's client URLs, such as tls://10.0.0.12:4224, a
	// node's IP. The client connects to any of them, and reconnects to any of
	// them.
	URLs []string

	// RootCAs returns the CAs that verify the coordinator's serving
	// certificate; the connection always uses TLS 1.3. It is called on every
	// connect and reconnect, so a replaced CA is trusted once the file it is
	// read from is refreshed. An error, or a nil pool, fails that handshake,
	// and the client retries it as any other failed connect.
	RootCAs func() (*x509.CertPool, error)

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
		return fmt.Errorf("no CA source for the coordinator's certificate: %w", session.ErrInvalid)
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
	// sends it again: of every operation but a read, which only its caller's
	// context and a disconnect bound.
	attemptTimeout = 3 * time.Second

	// noTimeout is how long nats.go lets a JetStream request run when the
	// request carries no deadline, as only a read does: without end, where
	// nats.go would stop it after 5 s.
	noTimeout = time.Duration(math.MaxInt64)
)

// Connect connects to the coordinator as cfg.Member of the attempt cfg.Attempt,
// and reads the session's Meta. An agent connects once for each member it
// hosts. It waits while the coordinator refuses the connection, which it does
// until it holds the session and has checked the token, and reports
// session.ErrForbidden, together with ctx's error, if ctx is done before it is
// admitted. Since the coordinator admits a joining member only once the
// session exists, it reports session.ErrNotFound only if the session is
// removed while the member joins.
//
// Connect reads cfg.RootCAs once before it first connects, and returns at once
// if that fails: a CA that cannot be read when the agent starts is not waited
// for. Once Connect has read it, a failed read fails only the handshake that
// needed it, which the client retries.
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
	s.life, s.endLife = context.WithCancel(context.Background())
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
		// Both ends are ours, so neither needs anything older than TLS 1.3.
		nats.Secure(&tls.Config{ServerName: cfg.ServerName, MinVersion: tls.VersionTLS13}),
		// The coordinator expects the handshake before it sends anything.
		nats.TLSHandshakeFirst(),
		// nats.go retries a handshake that fails here without reporting why, so
		// the client logs it. A nil pool would trust the host's CAs instead.
		nats.ClientTLSConfig(nil, func() (*x509.CertPool, error) {
			pool, err := cfg.RootCAs()
			if err == nil && pool == nil {
				err = fmt.Errorf("no CA pool for the coordinator's certificate: %w", session.ErrInvalid)
			}
			if err != nil {
				s.log.Error(err, "Reading the coordinator's CA failed; this connection attempt fails")
			}
			return pool, err
		}),
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
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			s.disconnected(nc)
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

// connection numbers the connection the client sends over now. nats.go counts
// every attempt to reconnect before the connection it makes is up, so a later
// connection has a higher number.
func (s *client) connection() uint64 { return s.nc.Stats().Reconnects }

// disconnected makes every operation sent over a connection that broke send
// its request again. nats.go reports a disconnect on a goroutine of its own,
// which may run once it has connected again: an operation sent over the new
// connection is then not lost, and is left alone. An operation is numbered
// just before it is sent, so one that a whole reconnect overtook in between
// is sent again all the same.
func (s *client) disconnected(nc *nats.Conn) {
	before := uint64(math.MaxUint64)
	if nc.IsConnected() {
		before = nc.Stats().Reconnects
	}
	s.inflight.interrupt(before)
}

// join waits until the coordinator admits the connection, binds the buckets,
// reads Meta, makes the Store of each namespace, and starts verifying the
// session when asked, following its end, and timing its deadline.
func (s *client) join(ctx context.Context) error {
	select {
	case <-s.connected:
	case <-ctx.Done():
		if errors.Is(s.nc.LastError(), nats.ErrAuthorization) {
			return fmt.Errorf("the coordinator refused the connection (%w) until: %w", session.ErrForbidden, ctx.Err())
		}
		return fmt.Errorf("connect to the coordinator: %w", ctx.Err())
	}
	js, err := jetstream.New(s.nc, jetstream.WithDefaultTimeout(noTimeout))
	if err != nil {
		return mapError(err)
	}
	buckets := make(map[string]bucket)
	for _, name := range session.Buckets(s.attempt) {
		if buckets[name], err = s.bind(ctx, js, name); err != nil {
			return err
		}
	}
	s.control = buckets[session.ControlBucket(s.attempt)]
	// What the coordinator buffers for the connection, session.MaxPending, is
	// MaxConcurrentGets of the largest messages it takes, session.MaxPayload,
	// which it announced as the connection began: the budget holds as much
	// before Meta is read.
	s.budget = semaphore.NewWeighted(session.MaxConcurrentGets * s.nc.MaxPayload())
	s.slots = semaphore.NewWeighted(session.MaxConcurrentGets)
	s.mu.Lock()
	target := s.target()
	s.mu.Unlock()
	// The membership, which bounds Meta, is Meta's to tell.
	var meta session.Meta
	if err := s.read(ctx, controlRead(session.MaxMembers), s.readMeta(&meta)); err != nil {
		return err
	}
	s.meta = meta
	s.ranks = make(map[string]int, len(s.meta.Members))
	for _, m := range s.meta.Members {
		s.ranks[m.ID] = m.Rank
	}
	if _, ok := s.ranks[s.member]; !ok {
		return fmt.Errorf("the session has no member %q: %w", s.member, session.ErrForbidden)
	}
	s.stores = make(map[session.Namespace]*store)
	for _, ns := range session.Namespaces() {
		name, err := ns.Bucket(s.attempt)
		if err != nil {
			return err
		}
		s.stores[ns] = s.newStore(ns, name, buckets[name])
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

// bind binds one of the attempt's buckets, as a key-value bucket and as the
// stream that holds it.
func (s *client) bind(ctx context.Context, js jetstream.JetStream, name string) (bucket, error) {
	var b bucket
	err := s.do(ctx, func(ctx context.Context) error {
		kv, err := js.KeyValue(ctx, name)
		if err != nil {
			return err
		}
		stream, err := js.Stream(ctx, "KV_"+name)
		if err != nil {
			return err
		}
		b = bucket{kv: kv, stream: stream}
		return nil
	})
	return b, err
}

// readMeta returns an operation that reads and decodes the meta key into meta,
// which a read of the control bucket runs.
func (s *client) readMeta(meta *session.Meta) func(context.Context) error {
	var r record
	read := latest(s.control, session.KeyMeta, &r)
	return func(ctx context.Context) error {
		err := read(ctx)
		if errors.Is(err, session.ErrNotFound) || err == nil && r.deleted {
			return fmt.Errorf("the session has no meta: %w", session.ErrNotFound)
		}
		if err != nil {
			return err
		}
		if err := json.Unmarshal(r.value, meta); err != nil {
			return fmt.Errorf("decode the session's meta: %w", err)
		}
		return nil
	}
}

// controlRead is what a read of the control bucket of a session of members
// members takes of the connection's budget: the most its reply holds.
func controlRead(members int) readCost {
	return readCost{bytes: int64(session.ControlValueBytes(members)) + session.ReplyHeadroom}
}

// Rank implements Session.
func (s *client) Rank() int { return s.ranks[s.member] }

// Meta implements Session.
func (s *client) Meta() session.Meta {
	meta := s.meta
	meta.Members = slices.Clone(s.meta.Members)
	return meta
}

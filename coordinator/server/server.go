// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/go-logr/logr"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
)

// DefaultMaxBytes is the default memory cap, which every session shares.
const DefaultMaxBytes = 1 << 30

// DefaultGrace is how long a member whose connection closed has to connect
// again before the coordinator fails its session with
// session.ReasonMemberFailed. A grace period starts only once the coordinator
// sees the connection close, and a client that has connected again by then
// never left its member without a connection. So what must fit within it is
// the session client noticing a dead connection and connecting again, which
// restores a connection that breaks while the agent and the coordinator keep
// running. How long the coordinator takes to notice a dead connection changes
// only how late a member that never comes back fails the attempt. It does not
// cover an agent restart, and is not meant to: a restarted agent never rejoins
// an attempt in flight.
const DefaultGrace = 20 * time.Second

// The coordinator's side of a member's keepalive. The coordinator gives up on
// a connection whose client stops answering after serverPingInterval ×
// (serverMaxPingsOut + 1), and the member's grace period starts once it sees
// the connection close.
const (
	// serverPingInterval is how often the coordinator pings a client.
	serverPingInterval = 5 * time.Second

	// serverMaxPingsOut is how many pings the coordinator leaves unanswered
	// before it closes a client's connection.
	serverMaxPingsOut = 2
)

// The accounts of the embedded server. Members and the coordinator's own
// connection share the application account, the only one with JetStream; the
// system account carries the server's events.
const (
	appAccount    = "SNAPSHOT"
	systemAccount = "SYS"
)

// The users of the coordinator's two in-process connections. No member's user
// name, which session.UserName makes, can take either form.
const (
	coordinatorUser = "_coordinator"
	eventsUser      = "_events"
)

// The embedded server's limits beyond those Config sets.
const (
	// payloadHeadroom is what a message may carry beyond its value: the key in
	// its subject does not count, but the headers of a write do.
	payloadHeadroom = 64 << 10

	// valueHeadroom is what a bucket's maximum message size adds to its
	// maximum value, for the headers a write carries. Meta carries the exact
	// value limits, which clients enforce themselves.
	valueHeadroom = 1 << 10

	// writeDeadline is how long the server waits for a client to take what it
	// sends before dropping the client as a slow consumer, which then gets the
	// grace period to reconnect.
	writeDeadline = 10 * time.Second

	// startTimeout bounds how long the embedded server may take to start.
	startTimeout = 10 * time.Second

	// drainTimeout bounds how long Shutdown waits for members to close their
	// connections once their sessions have failed, and drainPoll is how often
	// it checks.
	drainTimeout = 2 * time.Second
	drainPoll    = 20 * time.Millisecond
)

// Config carries a Server's settings and dependencies. TLS and Authorizer are
// required.
type Config struct {
	// Host and Port are where the client listener listens. Port defaults to
	// 4222, and natsserver.RANDOM_PORT picks a free one.
	Host string
	Port int

	// TLS is the client listener's TLS configuration, which must serve a
	// certificate. Every new connection is served the certificate this
	// configuration returns at that moment, so a GetCertificate that follows a
	// rotated certificate, such as controller-runtime's certwatcher, rotates
	// it without restarting anything.
	TLS *tls.Config

	// Authorizer decides who may act for which member. There is no allow-all
	// default, so a server never runs unauthenticated by omission.
	Authorizer Authorizer

	// MaxBytes is the only byte limit: it caps the memory of every session's
	// buckets together. Sessions share this memory and none reserves any, so
	// a session that fills it makes other sessions' writes fail with
	// session.ErrTooMany until it is removed. It defaults to DefaultMaxBytes,
	// and should be about half the container's memory limit.
	MaxBytes int64

	// MaxValue and MaxLibraryValue are the largest values of the Snapshot and
	// library buckets. They default to session.MaxValue and
	// session.MaxLibraryValue.
	MaxValue, MaxLibraryValue int

	// MaxLibraryKeys is how many keys each session's library bucket holds.
	// It defaults to session.MaxLibraryKeys.
	MaxLibraryKeys int

	// Grace is how long a member whose connection closed has to connect
	// again. It defaults to DefaultGrace.
	Grace time.Duration

	// Clock times deadlines and grace periods. It defaults to the real clock.
	Clock clock.WithDelayedExecution

	// Observer receives what sessions do. It defaults to reporting nothing.
	Observer Observer

	// Log receives the server's life, sessions opening and ending, members
	// refused, disconnected, and failed, and the embedded server's own log. It
	// is never given a key, a value, a barrier name, or a token. The zero
	// Logger discards everything.
	Log logr.Logger
}

// withDefaults validates cfg and fills in its defaults.
func (cfg Config) withDefaults() (Config, error) {
	switch {
	case cfg.TLS == nil:
		return cfg, fmt.Errorf("a coordinator needs a TLS configuration: %w", session.ErrInvalid)
	case cfg.Authorizer == nil:
		return cfg, fmt.Errorf("a coordinator needs an authorizer: %w", session.ErrInvalid)
	}
	setDefault(&cfg.MaxBytes, DefaultMaxBytes)
	setDefault(&cfg.MaxValue, session.MaxValue)
	setDefault(&cfg.MaxLibraryValue, session.MaxLibraryValue)
	setDefault(&cfg.MaxLibraryKeys, session.MaxLibraryKeys)
	setDefault(&cfg.Grace, DefaultGrace)
	switch {
	case cfg.MaxBytes < 0, cfg.MaxValue < 0, cfg.MaxLibraryValue < 0, cfg.MaxLibraryKeys < 0,
		cfg.Grace < 0:
		return cfg, fmt.Errorf("a coordinator's limits cannot be negative: %w", session.ErrInvalid)
	case cfg.MaxValue > 1<<30, cfg.MaxLibraryValue > cfg.MaxValue:
		return cfg, fmt.Errorf("the largest values must not exceed 1 GiB, nor the library's the Snapshot's: %w",
			session.ErrInvalid)
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.RealClock{}
	}
	if cfg.Observer == nil {
		cfg.Observer = nopObserver{}
	}
	return cfg, nil
}

// setDefault sets *v to def when it is zero.
func setDefault[T comparable](v *T, def T) {
	var zero T
	if *v == zero {
		*v = def
	}
}

// Server is the Snapshot coordinator: a NATS server with JetStream embedded in
// the process, which holds each attempt's session in two in-memory key-value
// buckets, authenticates every client, and enforces what a store lacks: the
// membership, the deadline, the grace period, and closing once every member
// has left. Package session, which it shares with the client, defines the
// layout, the limits, and the permissions a member's connection gets.
//
// The server starts refusing every client, and admits them only once the host
// calls MarkReady, which it does once it has ensured the session of every
// unfinished attempt. A coordinator restart loses every session, so until then
// a member reconnecting with the epoch it read from the session it joined would
// find nothing, fail nothing, and leave the members that had not connected yet
// to join the recreated session and wait for its deadline. Once ready, such a
// member finds the recreated session's other epoch, which fails it with
// session.ReasonCoordinatorLost at once.
//
// A Server is safe for concurrent use.
type Server struct {
	cfg      Config
	log      logr.Logger
	ns       *natsserver.Server
	storeDir string
	// token authenticates the coordinator's in-process connections. It is
	// drawn at random at startup and never leaves the process's memory.
	token   string
	account *natsserver.Account
	conn    *nats.Conn
	// connClosed is closed once conn is, by Shutdown; nats.go then delivers
	// nothing more to any watch.
	connClosed chan struct{}
	js         jetstream.JetStream
	events     *nats.Conn
	ready      atomic.Bool

	// control serializes Ensure, Remove, and Shutdown, which create and
	// delete buckets.
	control sync.Mutex

	// mu guards the fields below and every attempt's mutable state.
	mu       sync.Mutex
	attempts map[types.UID]*attempt
	shutdown bool
	wg       sync.WaitGroup
}

// New starts the embedded server, listening for clients and refusing
// them until MarkReady.
func New(cfg Config) (*Server, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return nil, fmt.Errorf("draw the coordinator's token: %w", err)
	}
	// JetStream checks that it can write here, although the buckets live in
	// memory only.
	storeDir, err := os.MkdirTemp("", "snapshot-coordinator-")
	if err != nil {
		return nil, fmt.Errorf("create the JetStream directory: %w", err)
	}
	s := &Server{
		cfg:        cfg,
		log:        cfg.Log,
		storeDir:   storeDir,
		token:      hex.EncodeToString(token),
		connClosed: make(chan struct{}),
		attempts:   make(map[types.UID]*attempt),
	}
	if err := s.start(); err != nil {
		s.stop()
		return nil, err
	}
	s.log.Info("Coordinator started; refusing clients until ready", "address", s.ns.Addr().String())
	return s, nil
}

// start starts the embedded server and the coordinator's own connections.
func (s *Server) start() error {
	maxPayload := int32(s.cfg.MaxValue + payloadHeadroom)
	opts := &natsserver.Options{
		ServerName: "snapshot-coordinator",
		Host:       s.cfg.Host,
		Port:       s.cfg.Port,
		// The process owns its signals.
		NoSigs: true,
		// Logs go through SetLoggerV2 below, never to standard error.
		NoLog:         true,
		TLSConfig:     s.cfg.TLS,
		MaxPayload:    maxPayload,
		MaxPending:    2 * int64(maxPayload),
		WriteDeadline: writeDeadline,
		PingInterval:  serverPingInterval,
		MaxPingsOut:   serverMaxPingsOut,
		JetStream:     true,
		// Buckets live in memory only: a store of one byte refuses every
		// file-backed stream.
		JetStreamMaxMemory:     s.cfg.MaxBytes,
		JetStreamMaxStore:      1,
		StoreDir:               s.storeDir,
		DisableJetStreamBanner: true,
		Accounts: []*natsserver.Account{
			natsserver.NewAccount(appAccount), natsserver.NewAccount(systemAccount),
		},
		SystemAccount:              systemAccount,
		CustomClientAuthentication: authenticator{s},
	}
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		return fmt.Errorf("configure the embedded server: %w", err)
	}
	ns.SetLoggerV2(natsLogger{s.log.WithName("nats")}, false, false, false)
	s.ns = ns
	ns.Start()
	if !ns.ReadyForConnections(startTimeout) {
		return errors.New("the embedded server did not start")
	}
	if s.account, err = ns.LookupAccount(appAccount); err != nil {
		return fmt.Errorf("look up the application account: %w", err)
	}
	if err := s.account.EnableJetStream(nil, nil); err != nil {
		return fmt.Errorf("enable JetStream: %w", err)
	}
	if s.conn, err = s.connect(coordinatorUser,
		nats.ClosedHandler(func(*nats.Conn) { close(s.connClosed) })); err != nil {
		return err
	}
	if s.js, err = jetstream.New(s.conn); err != nil {
		return fmt.Errorf("open JetStream: %w", err)
	}
	if s.events, err = s.connect(eventsUser); err != nil {
		return err
	}
	return s.followConnections()
}

// connect opens one of the coordinator's in-process connections.
func (s *Server) connect(user string, opts ...nats.Option) (*nats.Conn, error) {
	nc, err := nats.Connect("", append([]nats.Option{nats.InProcessServer(s.ns), nats.UserInfo(user, ""),
		nats.Token(s.token), nats.Name(user), nats.ErrorHandler(s.natsError)}, opts...)...)
	if err != nil {
		return nil, fmt.Errorf("open the coordinator's %s connection: %w", user, err)
	}
	return nc, nil
}

// natsError logs an error that nats.go reports on one of the coordinator's
// own connections, such as a watch whose consumer it could not recreate,
// which it would otherwise print to standard error. A refused publish or
// subscription names its subject, which can hold a key, so its text is never
// logged.
func (s *Server) natsError(_ *nats.Conn, _ *nats.Subscription, err error) {
	if errors.Is(err, nats.ErrPermissionViolation) {
		s.log.Info("The embedded server refused a publish or a subscription of the coordinator's")
		return
	}
	s.log.Error(err, "A connection of the coordinator's reported an error")
}

// MarkReady starts admitting clients. The host calls it once it has ensured
// the session of every unfinished attempt; see Server.
func (s *Server) MarkReady() {
	if !s.ready.Swap(true) {
		s.log.Info("Coordinator ready; admitting clients")
	}
}

// Addr returns the address the client listener listens on.
func (s *Server) Addr() net.Addr { return s.ns.Addr() }

// Shutdown ends the coordinator, when its host stops serving: it fails every
// open session with session.ReasonCoordinatorLost, waits briefly, within ctx,
// for the members to learn it and close their connections, then shuts the
// embedded server down, which closes every connection left. A member that
// missed the failure learns the same from the next coordinator, through the
// epoch. Ensure and Remove fail afterwards, and shutting down again does
// nothing.
func (s *Server) Shutdown(ctx context.Context) error {
	s.control.Lock()
	defer s.control.Unlock()
	s.mu.Lock()
	if s.shutdown {
		s.mu.Unlock()
		return nil
	}
	s.shutdown = true
	attempts := slices.Collect(maps.Values(s.attempts))
	s.mu.Unlock()
	s.ready.Store(false)

	var errs []error
	for _, a := range attempts {
		if err := s.endWith(ctx, a, []byte(session.ReasonCoordinatorLost)); err != nil && !ended(err) {
			errs = append(errs, fmt.Errorf("fail attempt %s: %w", a.spec.UID, err))
		}
	}
	s.awaitMembers(ctx)
	for _, a := range attempts {
		s.discard(a)
	}
	s.stop()
	s.log.Info("Coordinator shut down")
	return errors.Join(errs...)
}

// awaitMembers waits until no member is connected, for at most drainTimeout.
// A client closes its connection once its session has ended.
func (s *Server) awaitMembers(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, drainTimeout)
	defer cancel()
	ticker := time.NewTicker(drainPoll)
	defer ticker.Stop()
	// The coordinator's own connection is the account's last.
	for s.account.NumLocalConnections() > 1 {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// stop closes the coordinator's connections, shuts the embedded server down,
// and waits for everything the server started.
func (s *Server) stop() {
	if s.events != nil {
		s.events.Close()
	}
	if s.conn != nil {
		s.conn.Close()
	}
	if s.ns != nil {
		s.ns.Shutdown()
		s.ns.WaitForShutdown()
	}
	s.wg.Wait()
	_ = os.RemoveAll(s.storeDir)
}

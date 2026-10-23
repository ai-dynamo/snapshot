// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"context"
	"errors"
	"fmt"
	"time"

	wire "github.com/ai-dynamo/snapshot/agent/pkg/coordinator/v1alpha1"
	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/session"
	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
)

// DefaultGrace is how long a member whose stream was lost has to attach a new
// one before its session fails: the few seconds SNEP-324 allows a stream to
// break while the agent and the coordinator keep running.
const DefaultGrace = 5 * time.Second

// Authorizer decides whether the peer behind a stream may act for a member of
// an attempt. It is consulted once per stream, before anything touches the
// session, and its error says what the client does next. The server passes
// these codes through unchanged:
//
//   - Unauthenticated or PermissionDenied: a refusal. The client stops.
//   - Unavailable: a transient failure, such as a failed TokenReview or API
//     call. The client retries.
//   - NotFound: the attempt is not known yet, including when the Authorizer's
//     own cache lags. The client retries.
//
// Any other error, plain or with another status, ends the stream with
// Unavailable and is logged. Nothing is granted without a nil error, so a real
// refusal must use one of the first two codes.
//
// A member's grace period runs from losing its stream to registering a new one,
// authorization included, so Authorize must answer within a small fraction of
// the grace period, for example by caching TokenReviews.
type Authorizer interface {
	Authorize(ctx context.Context, uid types.UID, member session.MemberID) error
}

// Config carries a Server's dependencies. Manager and Authorizer are required.
type Config struct {
	// Manager holds the sessions the server serves.
	Manager *session.Manager

	// Authorizer decides who may act for which member. There is no allow-all
	// default, so a server never runs unauthenticated by omission.
	Authorizer Authorizer

	// Clock times the grace period. It defaults to the real clock.
	Clock clock.WithDelayedExecution

	// Grace is how long a lost member has to reattach. It defaults to
	// DefaultGrace.
	Grace time.Duration

	// Log receives attaches, reattaches, superseded streams, grace expiries,
	// and internal errors, never keys, values, barrier names, or participants.
	// The zero Logger discards everything.
	Log logr.Logger
}

// Server serves sessions over the Coordinator service. Register it with
// coordinatorv1alpha1.RegisterCoordinatorServer, on a grpc.Server that:
//
//   - accepts messages of coordinatorv1alpha1.MaxMessageSize, with
//     grpc.MaxRecvMsgSize;
//   - pings idle connections and closes those that stop answering, with
//     grpc.KeepaliveParams, so a half-open stream is detected, and its member's
//     grace period started, within seconds. Without it, the stream of a lost
//     node may never end, and a send blocked on it never returns.
type Server struct {
	wire.UnimplementedCoordinatorServer

	manager    *session.Manager
	authorizer Authorizer
	log        logr.Logger
	registry   *registry
}

// NewServer returns a Server for cfg.
func NewServer(cfg Config) (*Server, error) {
	if cfg.Manager == nil {
		return nil, errors.New("a coordinator server needs a session manager")
	}
	if cfg.Authorizer == nil {
		return nil, errors.New("a coordinator server needs an authorizer")
	}
	if cfg.Grace < 0 {
		return nil, fmt.Errorf("a coordinator server's grace period cannot be negative: %s", cfg.Grace)
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.RealClock{}
	}
	if cfg.Grace == 0 {
		cfg.Grace = DefaultGrace
	}
	return &Server{
		manager:    cfg.Manager,
		authorizer: cfg.Authorizer,
		log:        cfg.Log,
		registry:   newRegistry(cfg.Manager, cfg.Clock, cfg.Grace, cfg.Log),
	}, nil
}

// Attach implements coordinatorv1alpha1.CoordinatorServer: it carries one
// member's part in one attempt, as coordinator.proto specifies.
func (s *Server) Attach(grpcStream wire.Coordinator_AttachServer) error {
	ctx := grpcStream.Context()
	hello, err := receiveHello(grpcStream)
	if err != nil {
		return err
	}
	seq := s.registry.arrive()
	uid, member := types.UID(hello.GetSession()), session.MemberID(hello.GetMember())
	log := s.log.WithValues("attempt", uid, "member", member)
	if err := s.authorize(ctx, log, uid, member); err != nil {
		return err
	}

	sess, first, err := s.admit(ctx, log, hello)
	if err != nil {
		return err
	}
	if sess == nil {
		// The session has ended, or the coordinator lost it: Ended is the
		// whole answer.
		return grpcStream.Send(first)
	}
	// A client that gave up while its stream was authorized and admitted has
	// abandoned it, so the stream must not end the member's grace period, or
	// supersede a stream the client opened since.
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}

	st := newStream(grpcStream, sess, member, seq, log)
	if !s.registry.register(st) {
		log.Info("A newer stream for the same member attached first; ending this one")
		return status.Error(codes.Aborted, "a newer stream for this member won")
	}
	if hello.GetEpoch() == 0 {
		log.Info("Member attached")
	} else {
		log.Info("Member reattached")
	}
	exit, err := st.run(first)
	switch exit {
	case exitEnded:
		s.registry.ended(st)
	case exitLost:
		s.registry.lose(st)
	case exitSuperseded:
		// The newer stream already owns the member's registration.
	}
	return err
}

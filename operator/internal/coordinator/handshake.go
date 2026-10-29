// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"context"
	"errors"
	"io"

	wire "github.com/ai-dynamo/snapshot/agent/pkg/coordinator/v1alpha1"
	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/session"
	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/types"
)

// receiveHello reads the stream's first message, which must be a well-formed
// Hello.
func receiveHello(grpcStream wire.Coordinator_AttachServer) (*wire.Hello, error) {
	msg, err := grpcStream.Recv()
	if errors.Is(err, io.EOF) {
		return nil, status.Error(codes.InvalidArgument, "the stream closed before Hello")
	}
	if err != nil {
		return nil, err
	}
	hello := msg.GetHello()
	switch {
	case hello == nil:
		return nil, status.Error(codes.InvalidArgument, "the first message must be Hello")
	case hello.GetSession() == "":
		return nil, status.Error(codes.InvalidArgument, "a Hello without a session")
	case hello.GetMember() == "":
		return nil, status.Error(codes.InvalidArgument, "a Hello without a member")
	}
	return hello, nil
}

// authorize asks the Authorizer whether the peer may act for member. The codes
// of the Authorizer's contract pass through unchanged; any other error becomes
// Unavailable, and is logged unless the client gave up first.
func (s *Server) authorize(ctx context.Context, log logr.Logger, uid types.UID, member session.MemberID) error {
	err := s.authorizer.Authorize(ctx, uid, member)
	if err == nil {
		return nil
	}
	switch status.Code(err) {
	case codes.Unauthenticated, codes.PermissionDenied, codes.Unavailable, codes.NotFound:
		return err
	}
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	log.Error(err, "The Authorizer failed outside its contract; the client will retry")
	return status.Error(codes.Unavailable, "authorization is unavailable")
}

// admit decides what a stream's Hello gets, following the epoch rules of
// coordinator.proto. It returns the session and its Welcome for a stream that
// takes part; a nil session and Ended for one that only learns the attempt is
// over; or a gRPC status for one that cannot carry the session at all.
func (s *Server) admit(ctx context.Context, log logr.Logger, hello *wire.Hello) (*session.Session,
	*wire.ServerMessage, error) {
	uid := types.UID(hello.GetSession())
	agent := session.Caller{Member: session.MemberID(hello.GetMember()), Role: session.RoleAgent}
	epoch := session.Epoch(hello.GetEpoch())
	sess, err := s.manager.Session(uid)
	if err != nil {
		if epoch == 0 {
			return nil, nil, status.Errorf(codes.NotFound, "the coordinator holds no session for attempt %s yet", uid)
		}
		return nil, endedMessage(errCoordinatorLost), nil
	}

	// Attaching first refuses a peer the session has no member for, and reports
	// the session's end if it already ended, before a stale epoch can fail
	// anything.
	welcome, err := sess.Attach(agent)
	if err != nil {
		return s.refuse(log, err)
	}
	if epoch != 0 && epoch != s.manager.Epoch() {
		// The member attached to an earlier coordinator, which took whatever
		// the member had with it. The session cannot recover that, so it
		// fails now rather than at its deadline.
		if err := s.manager.Fail(ctx, uid, session.ReasonCoordinatorLost); err != nil {
			return s.refuse(log, err)
		}
		log.Info("Failed the session for a member that attached to an earlier coordinator")
		return nil, endedMessage(errCoordinatorLost), nil
	}
	return sess, welcomeMessage(welcome), nil
}

// errCoordinatorLost is the end Ended reports when the coordinator lost the
// session, or what the member had in it.
var errCoordinatorLost = &session.AbortError{Reason: session.ReasonCoordinatorLost}

// refuse answers a Hello the session turned down: Ended when the session has
// ended or is gone, and a gRPC status otherwise.
func (s *Server) refuse(log logr.Logger, err error) (*session.Session, *wire.ServerMessage, error) {
	switch {
	case errors.Is(err, session.ErrAborted), errors.Is(err, session.ErrClosed):
		return nil, endedMessage(err), nil
	case errors.Is(err, session.ErrUnknownSession):
		// Removed since the Hello arrived.
		return nil, endedMessage(errCoordinatorLost), nil
	case errors.Is(err, session.ErrNotMember):
		return nil, nil, status.Error(codes.PermissionDenied, err.Error())
	default:
		log.Error(rootCause(err), "Admitting a stream failed in a way the Hello could not cause")
		return nil, nil, status.Error(codes.Internal, "admitting the stream failed")
	}
}

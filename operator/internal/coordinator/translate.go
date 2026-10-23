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
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// callerOf translates a request's origin into the caller the session judges.
// The translation is total: every role maps to its session role, and one left
// unspecified is invalid. Whether the caller reaches the key, or may leave or
// fail, is the session's to decide.
func callerOf(member session.MemberID, origin *wire.Origin) (session.Caller, error) {
	caller := session.Caller{Member: member}
	switch origin.GetRole() {
	case wire.Role_ROLE_AGENT:
		caller.Role = session.RoleAgent
	case wire.Role_ROLE_RELAY:
		caller.Role = session.RoleRelay
	case wire.Role_ROLE_LIBRARY:
		caller.Role = session.RoleLibrary
		caller.Library, caller.Version = origin.GetLibrary(), origin.GetVersion()
	default:
		return session.Caller{}, fmt.Errorf("a request from origin role %s: %w", origin.GetRole(), session.ErrInvalid)
	}
	return caller, nil
}

// waitOf translates a Get's wait. An absent wait is zero; a negative or
// malformed one is invalid rather than read as zero, so a client's mistake
// surfaces instead of turning a wait into a lookup.
func waitOf(d *durationpb.Duration) (time.Duration, error) {
	if d == nil {
		return 0, nil
	}
	if err := d.CheckValid(); err != nil {
		return 0, fmt.Errorf("a malformed wait (%v): %w", err, session.ErrInvalid)
	}
	wait := d.AsDuration()
	if wait < 0 {
		return 0, fmt.Errorf("a negative wait of %s: %w", wait, session.ErrInvalid)
	}
	return wait, nil
}

// errorOf translates an error into the Error a client acts on. The translation
// is total. An abort carries its reason. A context error is CANCELLED: the
// session reports its own end as the abort or as ErrClosed, so a context error
// can only mean the request's own context was done, because its client
// cancelled it, its stream ended, or its stream's deadline passed. Every
// sentinel an operation can report has its code, and anything else is
// INTERNAL. ErrUnknownSession is not here, because only a Hello can meet it.
func errorOf(err error) *wire.Error {
	e := &wire.Error{Code: codeOf(err), Message: err.Error()}
	var aborted *session.AbortError
	if errors.As(err, &aborted) {
		e.Reason = string(aborted.Reason)
	}
	return e
}

// codeOf returns the code for an error.
func codeOf(err error) wire.Code {
	switch {
	case errors.Is(err, session.ErrAborted):
		return wire.Code_CODE_ABORTED
	case errors.Is(err, session.ErrClosed):
		return wire.Code_CODE_CLOSED
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return wire.Code_CODE_CANCELLED
	case errors.Is(err, session.ErrNotFound):
		return wire.Code_CODE_NOT_FOUND
	case errors.Is(err, session.ErrNotYet):
		return wire.Code_CODE_NOT_YET
	case errors.Is(err, session.ErrExists):
		return wire.Code_CODE_EXISTS
	case errors.Is(err, session.ErrMismatch):
		return wire.Code_CODE_MISMATCH
	case errors.Is(err, session.ErrNotCounter):
		return wire.Code_CODE_NOT_COUNTER
	case errors.Is(err, session.ErrBarrierCount):
		return wire.Code_CODE_BARRIER_COUNT
	case errors.Is(err, session.ErrNotMember):
		return wire.Code_CODE_NOT_MEMBER
	case errors.Is(err, session.ErrForbidden):
		return wire.Code_CODE_FORBIDDEN
	case errors.Is(err, session.ErrTooLarge):
		return wire.Code_CODE_TOO_LARGE
	case errors.Is(err, session.ErrTooMany):
		return wire.Code_CODE_TOO_MANY
	case errors.Is(err, session.ErrInvalid):
		return wire.Code_CODE_INVALID
	default:
		return wire.Code_CODE_INTERNAL
	}
}

// rootCause returns the innermost error of a chain, for a log. The session
// wraps its errors with the keys and names they concern, which a log must not
// carry; the cause beneath carries none of the session's own wrapping.
func rootCause(err error) error {
	for {
		inner := errors.Unwrap(err)
		if inner == nil {
			return err
		}
		err = inner
	}
}

// welcomeMessage translates the session's Welcome.
func welcomeMessage(w session.Welcome) *wire.ServerMessage {
	members := make([]*wire.Member, 0, len(w.Members))
	for _, m := range w.Members {
		members = append(members, &wire.Member{Id: string(m.ID), Rank: int32(m.Rank)})
	}
	return &wire.ServerMessage{Message: &wire.ServerMessage_Welcome{Welcome: &wire.Welcome{
		Epoch:    uint64(w.Epoch),
		Rank:     int32(w.Rank),
		Members:  members,
		Deadline: timestamppb.New(w.Deadline),
	}}}
}

// endedMessage translates how a session ended.
func endedMessage(err error) *wire.ServerMessage {
	return &wire.ServerMessage{Message: &wire.ServerMessage_Ended{Ended: &wire.Ended{Error: errorOf(err)}}}
}

// responseMessage wraps a response for the stream.
func responseMessage(resp *wire.Response) *wire.ServerMessage {
	return &wire.ServerMessage{Message: &wire.ServerMessage_Response{Response: resp}}
}

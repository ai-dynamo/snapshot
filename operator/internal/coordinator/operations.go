// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"context"
	"fmt"

	wire "github.com/ai-dynamo/snapshot/agent/pkg/coordinator/v1alpha1"
	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/session"
)

// execute runs one request against the session and answers it.
func (st *stream) execute(ctx context.Context, req *wire.Request) *wire.Response {
	resp, err := st.apply(ctx, req)
	if err != nil {
		resp = &wire.Response{Result: st.errorResult(req, err)}
	}
	resp.Id = req.GetId()
	return resp
}

// apply makes the session call a request asks for, and returns the response
// that carries its result. The session decides everything, from the namespace
// the origin reaches to who may leave or fail; apply only translates.
func (st *stream) apply(ctx context.Context, req *wire.Request) (*wire.Response, error) {
	caller, err := callerOf(st.member, req.GetOrigin())
	if err != nil {
		return nil, err
	}
	r := session.Request{Caller: caller, ID: session.RequestID(req.GetId())}
	switch op := req.GetOperation().(type) {
	case *wire.Request_Insert:
		return &wire.Response{}, st.session.Insert(ctx, r, op.Insert.GetKey(), op.Insert.GetValue())
	case *wire.Request_Get:
		wait, err := waitOf(op.Get.GetWait())
		if err != nil {
			return nil, err
		}
		value, err := st.session.Get(ctx, caller, op.Get.GetKey(), wait)
		return &wire.Response{Result: &wire.Response_Value{Value: value}}, err
	case *wire.Request_Unset:
		return &wire.Response{}, st.session.Unset(ctx, r, op.Unset.GetKey())
	case *wire.Request_Increment:
		counter, err := st.session.Increment(ctx, r, op.Increment.GetKey())
		return &wire.Response{Result: &wire.Response_Counter{Counter: counter}}, err
	case *wire.Request_Decrement:
		counter, err := st.session.Decrement(ctx, r, op.Decrement.GetKey())
		return &wire.Response{Result: &wire.Response_Counter{Counter: counter}}, err
	case *wire.Request_CompareAndSwap:
		cas := op.CompareAndSwap
		return &wire.Response{}, st.session.CompareAndSwap(ctx, r, cas.GetKey(), cas.GetExpected(), cas.GetValue())
	case *wire.Request_CreateBarrier:
		return &wire.Response{}, st.session.CreateBarrier(ctx, r, op.CreateBarrier.GetName(),
			int(op.CreateBarrier.GetCount()))
	case *wire.Request_PostBarrier:
		return &wire.Response{}, st.session.PostBarrier(ctx, r, op.PostBarrier.GetName(),
			session.ParticipantID(op.PostBarrier.GetParticipant()))
	case *wire.Request_AwaitBarrier:
		return &wire.Response{}, st.session.AwaitBarrier(ctx, caller, op.AwaitBarrier.GetName())
	case *wire.Request_Leave:
		return &wire.Response{}, st.session.Leave(ctx, caller)
	case *wire.Request_Fail:
		return &wire.Response{}, st.session.Fail(ctx, caller, session.Reason(op.Fail.GetReason()))
	default:
		return nil, fmt.Errorf("a request with no operation: %w", session.ErrInvalid)
	}
}

// errorResult translates an error into a response's result, and logs one the
// client could not have caused, without anything the request named.
func (st *stream) errorResult(req *wire.Request, err error) *wire.Response_Error {
	e := errorOf(err)
	if e.GetCode() == wire.Code_CODE_INTERNAL {
		st.log.Error(rootCause(err), "Request failed in a way the client could not cause",
			"role", req.GetOrigin().GetRole().String(), "operation", operationOf(req), "code", e.GetCode().String())
	}
	return &wire.Response_Error{Error: e}
}

// operationOf names a request's operation for a log, from the protocol's own
// field name.
func operationOf(req *wire.Request) string {
	m := req.ProtoReflect()
	field := m.WhichOneof(m.Descriptor().Oneofs().ByName("operation"))
	if field == nil {
		return "none"
	}
	return string(field.Name())
}

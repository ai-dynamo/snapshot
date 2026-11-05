// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// The JetStream API error codes a session maps onto package session's errors.
const (
	// jsMessageTooLarge: a message over the stream's maximum message size,
	// which is a bucket's maximum value size.
	jsMessageTooLarge = 10054
	// jsStoreFailed: the store refused a message. A session's only store
	// limit is the library bucket's number of keys, so this is a full bucket.
	jsStoreFailed = 10077
	// jsInsufficientResources: the coordinator's memory cap is reached.
	jsInsufficientResources = 10023
	// jsMaximumConsumers: a watch would take its bucket past its most
	// consumers, which the client waits out.
	jsMaximumConsumers = 10026
)

// mapError turns an error from nats.go into one of package session's, so no
// NATS type reaches a caller: errors are matched by JetStream error code, never
// by their text. Errors of package session and the standard library pass
// through; any other becomes an opaque error carrying only its text.
func mapError(err error) error {
	switch {
	case err == nil, ours(err), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, nats.ErrMaxPayload):
		return fmt.Errorf("the message is over the coordinator's largest: %w", session.ErrTooLarge)
	case errors.Is(err, nats.ErrConnectionClosed), errors.Is(err, jetstream.ErrConnectionClosed):
		return net.ErrClosed
	}
	switch code, description := apiError(err); code {
	case jsMessageTooLarge:
		return fmt.Errorf("the value is over its bucket's maximum: %w", session.ErrTooLarge)
	case jsStoreFailed, jsInsufficientResources:
		return fmt.Errorf("%s: %w", description, session.ErrTooMany)
	}
	return errors.New("coordinator: " + err.Error())
}

// apiError returns the code and description of the JetStream API error err
// carries, or zero. nats.go has two types for one: a key-value watch creates
// its consumer through nats.go's older API, which reports a *nats.APIError.
func apiError(err error) (int, string) {
	if apiErr, ok := errors.AsType[*jetstream.APIError](err); ok {
		return int(apiErr.ErrorCode), apiErr.Description
	}
	if apiErr, ok := errors.AsType[*nats.APIError](err); ok {
		return int(apiErr.ErrorCode), apiErr.Description
	}
	return 0, ""
}

// ours reports whether err is, or wraps, an error of package session.
func ours(err error) bool {
	var abort *session.AbortError
	if errors.As(err, &abort) {
		return true
	}
	for _, target := range []error{session.ErrExists, session.ErrNotFound, session.ErrNotYet, session.ErrClosed,
		session.ErrForbidden, session.ErrTooLarge, session.ErrTooMany, session.ErrInvalid, net.ErrClosed} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// transient reports whether an operation that failed with err may succeed if
// sent again: the connection broke, including under a write in progress, which
// fails with a *net.OpError, or is reconnecting, which refuses every publish
// since the client buffers none, the server did not answer in time, or a watch
// found its bucket at its most consumers, which free up as other waits end. An
// operation is sent again only while the session is alive, so a
// *net.OpError that wraps net.ErrClosed never reports a closed session.
func transient(err error) bool {
	if code, _ := apiError(err); code == jsMaximumConsumers {
		return true
	}
	if _, ok := errors.AsType[*net.OpError](err); ok {
		return true
	}
	return errors.Is(err, errRetry) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, nats.ErrTimeout) ||
		errors.Is(err, nats.ErrConnectionReconnecting) ||
		errors.Is(err, nats.ErrReconnectBufExceeded) ||
		errors.Is(err, nats.ErrDisconnected)
}

// missing reports whether an operation that failed with err may have found
// its bucket gone: no stream answered a write or a read, or the stream does not
// exist.
func missing(err error) bool {
	return errors.Is(err, nats.ErrNoResponders) ||
		errors.Is(err, jetstream.ErrNoStreamResponse) ||
		errors.Is(err, jetstream.ErrStreamNotFound) ||
		errors.Is(err, jetstream.ErrBucketNotFound) ||
		errors.Is(err, nats.ErrStreamNotFound)
}

// errRetry asks for an operation to be sent again at once, such as an Insert
// whose key was deleted between its Create and its Get.
var errRetry = errors.New("retry")

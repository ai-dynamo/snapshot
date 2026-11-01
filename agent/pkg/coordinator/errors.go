// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/ai-dynamo/snapshot/agent/pkg/session"
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
)

// mapError turns an error from nats.go into one of package session's, so no
// NATS type reaches a caller: errors are matched by JetStream error code, never
// by their text. Errors of package session and the standard library pass
// through; any other becomes an opaque error carrying only its text.
func mapError(err error) error {
	var apiErr *jetstream.APIError
	switch {
	case err == nil, ours(err), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, nats.ErrMaxPayload):
		return fmt.Errorf("the message is over the coordinator's largest: %w", session.ErrTooLarge)
	case errors.Is(err, nats.ErrConnectionClosed), errors.Is(err, jetstream.ErrConnectionClosed):
		return net.ErrClosed
	case errors.As(err, &apiErr):
		switch apiErr.ErrorCode {
		case jsMessageTooLarge:
			return fmt.Errorf("the value is over its bucket's maximum: %w", session.ErrTooLarge)
		case jsStoreFailed, jsInsufficientResources:
			return fmt.Errorf("%s: %w", apiErr.Description, session.ErrTooMany)
		}
	}
	return errors.New("coordinator: " + err.Error())
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
// sent again: the connection broke or is reconnecting, or the server did not
// answer in time.
func transient(err error) bool {
	return errors.Is(err, errRetry) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, nats.ErrTimeout) ||
		errors.Is(err, nats.ErrConnectionReconnecting) ||
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

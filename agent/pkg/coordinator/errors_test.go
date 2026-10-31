// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/ai-dynamo/snapshot/agent/pkg/session"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMapError covers how NATS's errors become package session's, with no NATS
// type left in any of them.
func TestMapError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want error
	}{
		{"a message over the largest", nats.ErrMaxPayload, session.ErrTooLarge},
		{"a value over the bucket's maximum", &jetstream.APIError{ErrorCode: jsMessageTooLarge}, session.ErrTooLarge},
		{"a full bucket", &jetstream.APIError{ErrorCode: jsStoreFailed}, session.ErrTooMany},
		{"a server out of memory", &jetstream.APIError{ErrorCode: jsInsufficientResources}, session.ErrTooMany},
		{"a closed connection", nats.ErrConnectionClosed, net.ErrClosed},
		{"a deadline", context.DeadlineExceeded, context.DeadlineExceeded},
		{"an error of package session", session.ErrNotYet, session.ErrNotYet},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.ErrorIs(t, mapError(tt.err), tt.want)
		})
	}
	for _, err := range []error{nats.ErrBadSubject, &jetstream.APIError{ErrorCode: 1}, jetstream.ErrBadBucket} {
		mapped := mapError(err)
		for e := mapped; e != nil; e = errors.Unwrap(e) {
			assert.NotContains(t, fmt.Sprintf("%T", e), "nats", "%v leaks a NATS type", err)
			assert.NotContains(t, fmt.Sprintf("%T", e), "jetstream", "%v leaks a NATS type", err)
		}
	}
}

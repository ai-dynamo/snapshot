// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"errors"
	"fmt"

	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv"
)

// The errors storage reports, under the names callers of this package use, so
// that the layer above maps one set of sentinels whatever the backend.
var (
	// ErrNotFound reports that a key does not exist and the caller did not ask
	// to wait for it.
	ErrNotFound = kv.ErrNotFound

	// ErrExists reports that a key already holds a different value. Inserting
	// the value a key already holds succeeds, so a retried insert is safe.
	ErrExists = kv.ErrExists

	// ErrMismatch reports that a compare-and-swap found a value other than the
	// one the caller expected.
	ErrMismatch = kv.ErrMismatch

	// ErrNotCounter reports that a key holds a value that is not a counter.
	ErrNotCounter = kv.ErrNotCounter

	// ErrBarrierCount reports that a barrier count is not positive, or that it
	// differs from the count the barrier already has.
	ErrBarrierCount = kv.ErrBarrierCount
)

// The errors this package reports itself. Callers match them with errors.Is;
// none of them is ever reported by comparing an error string.
var (
	// ErrNotYet reports that a key had not appeared when the caller's wait
	// expired. The attempt may still insert it.
	ErrNotYet = errors.New("key not set yet")

	// ErrAborted reports that the session failed. Every operation that reports
	// it does so through an *AbortError naming the reason.
	ErrAborted = errors.New("session aborted")

	// ErrClosed reports that the session closed because every member left.
	ErrClosed = errors.New("session closed")

	// ErrUnknownSession reports an attempt the Manager holds no session for:
	// one never ensured, or one already removed.
	ErrUnknownSession = errors.New("unknown session")

	// ErrNotMember reports that the caller is not a member of the session.
	ErrNotMember = errors.New("not a member of the session")

	// ErrForbidden reports that the caller's role does not reach the key, or
	// may not do what it asked.
	ErrForbidden = errors.New("forbidden for the caller's role")

	// ErrTooLarge reports that a value exceeds the limit of the caller's role,
	// or that a key, a barrier name, or a participant exceeds MaxKeyLength.
	ErrTooLarge = errors.New("too large")

	// ErrTooMany reports that library callers hold as much as one session
	// allows them: keys, bytes, barriers, participants, or recorded requests.
	ErrTooMany = errors.New("library quota reached")

	// ErrInvalid reports a malformed argument, or a spec that contradicts the
	// one a session was ensured with.
	ErrInvalid = errors.New("invalid argument")
)

// Reason names why a session failed. Members supply their own; the reasons
// below are the ones the engine supplies itself, and they match the condition
// reasons the operator reports on the set resource.
type Reason string

// The reasons the engine fails a session with.
const (
	// ReasonDeadlineExceeded is the reason the deadline fails a session with.
	ReasonDeadlineExceeded Reason = "DeadlineExceeded"

	// ReasonCancelled is the reason Manager.Remove fails a session still open
	// with, which under its rule means the set resource was deleted.
	ReasonCancelled Reason = "Cancelled"

	// ReasonCoordinatorLost is the reason to pass Manager.Shutdown when the
	// coordinator terminates, which members treat as losing it.
	ReasonCoordinatorLost Reason = "CoordinatorLost"
)

// AbortError reports that the session failed, and carries the reason. It
// unwraps to ErrAborted, so errors.Is holds for every aborted operation while
// errors.As recovers the reason.
type AbortError struct {
	// Reason is why the session failed.
	Reason Reason
}

// Error implements error.
func (e *AbortError) Error() string {
	if e.Reason == "" {
		return ErrAborted.Error()
	}
	return fmt.Sprintf("%s: %s", ErrAborted, e.Reason)
}

// Unwrap reports ErrAborted, which every aborted operation matches.
func (e *AbortError) Unwrap() error { return ErrAborted }

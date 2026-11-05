// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"errors"
	"fmt"
	"regexp"
)

// The errors of a session, which the client and the coordinator both report,
// so a caller matches one set whichever side reports it. Callers match them
// with errors.Is; none is ever reported by comparing an error string.
var (
	// ErrExists reports that a key already holds a different value. Inserting
	// the value a key already holds succeeds, so a repeated Insert is safe.
	ErrExists = errors.New("key holds a different value")

	// ErrNotFound reports that a key does not exist and the caller did not ask
	// to wait for it, or that an attempt has no session.
	ErrNotFound = errors.New("not found")

	// ErrNotYet reports that a key had not been inserted when the caller's wait
	// expired. The attempt may still insert it.
	ErrNotYet = errors.New("key not inserted yet")

	// ErrClosed reports that the session closed because every member left.
	ErrClosed = errors.New("session closed")

	// ErrForbidden reports that the coordinator refused the connection, or
	// that the session has no such member.
	ErrForbidden = errors.New("forbidden")

	// ErrTooLarge reports a value over its bucket's maximum, a message over the
	// coordinator's largest, or a key over the client's longest.
	ErrTooLarge = errors.New("too large")

	// ErrTooMany reports that a new key would take the library bucket past its
	// most keys, or that the coordinator's memory cap, which every session
	// shares, is reached.
	ErrTooMany = errors.New("bucket full")

	// ErrInvalid reports a malformed argument: a key, a name, a reason, a
	// namespace, or a configuration.
	ErrInvalid = errors.New("invalid argument")

	// ErrCoordinatorLost reports that the coordinator no longer holds the
	// session, because it restarted, or removed the session before this member
	// received its end: the member found a session with another epoch, or
	// none. It is an *AbortError, so it also matches a session that the
	// coordinator failed with ReasonCoordinatorLost: one it shut down under,
	// or recreated after a restart, which it fails as soon as a member that
	// joined the lost one reconnects.
	ErrCoordinatorLost error = &AbortError{Reason: ReasonCoordinatorLost}
)

// AbortError reports that the session failed, and carries the reason. Two
// AbortErrors match under errors.Is when their reasons are equal.
type AbortError struct {
	// Reason is why the session failed.
	Reason string
}

// Error implements error.
func (e *AbortError) Error() string { return "session failed: " + e.Reason }

// Is reports whether target is an *AbortError with the same reason.
func (e *AbortError) Is(target error) bool {
	t, ok := target.(*AbortError)
	return ok && t.Reason == e.Reason
}

// The reasons a session fails with besides those a member supplies to Fail:
// the coordinator's, and the one that replaces an ended key that holds no
// reason. They match the condition
// reasons the operator reports on the set resource.
const (
	// ReasonDeadlineExceeded is the reason the coordinator fails a session
	// with when its deadline passes.
	ReasonDeadlineExceeded = "DeadlineExceeded"

	// ReasonMemberFailed is the reason the coordinator fails a session with
	// when a member's connection stays closed past the grace period.
	ReasonMemberFailed = "MemberFailed"

	// ReasonCoordinatorLost is the reason a session fails with when the
	// coordinator holding it goes away.
	ReasonCoordinatorLost = "CoordinatorLost"

	// ReasonCancelled is the reason the coordinator fails a session still open
	// when it removes it, which means the set resource was deleted.
	ReasonCancelled = "Cancelled"

	// ReasonInvalid is the reason EndedError reports for a session whose ended
	// key holds bytes that are not a valid condition reason, which any
	// member's connection may write.
	ReasonInvalid = "InvalidReason"
)

// The rule for a Kubernetes condition reason, which apimachinery holds but does
// not export.
const (
	reasonPattern   = `^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$`
	maxReasonLength = 1024
)

var reasonRE = regexp.MustCompile(reasonPattern)

// ValidateReason reports ErrInvalid for a reason that is not a valid Kubernetes
// condition reason, so every reason can be reported as one and used as a metric
// label.
func ValidateReason(reason string) error {
	if len(reason) > maxReasonLength {
		return fmt.Errorf("a reason of %d bytes is over the %d-byte limit: %w", len(reason), maxReasonLength,
			ErrInvalid)
	}
	if !reasonRE.MatchString(reason) {
		return fmt.Errorf("reason %q is not a condition reason: %w", reason, ErrInvalid)
	}
	return nil
}

// EndedError returns how a session ended, from the value of its ended key, and
// is the one place either side decodes that value: ErrClosed for no bytes, an
// *AbortError with the reason for a valid condition reason, and an *AbortError
// with ReasonInvalid for anything else. Any member's connection may write the
// key, with up to the bucket's largest value, so the value is never trusted.
func EndedError(value []byte) error {
	switch {
	case len(value) == 0:
		return ErrClosed
	case len(value) > maxReasonLength || !reasonRE.Match(value):
		return &AbortError{Reason: ReasonInvalid}
	}
	return &AbortError{Reason: string(value)}
}

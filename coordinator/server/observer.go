// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import "k8s.io/apimachinery/pkg/types"

// Observer receives what sessions do, for metrics. A reason is a valid
// condition reason, so it is a bounded metric label: a session whose ended key
// holds anything else is reported with session.ReasonInvalid.
//
// A Server calls an Observer from the goroutine that ended a session, or that
// saw it end, and never while holding the lock that guards sessions' state. An
// implementation must be safe for concurrent use, must not block, and must not
// call back into the Server.
type Observer interface {
	// SessionOpened reports a session created for an attempt.
	SessionOpened(uid types.UID)

	// SessionClosed reports a session that ended because every member left.
	SessionClosed(uid types.UID)

	// SessionFailed reports a session that ended with a reason.
	SessionFailed(uid types.UID, reason string)
}

// nopObserver is the default: a Server with no Observer configured reports
// nothing. Metrics arrive with the coordinator binary.
type nopObserver struct{}

func (nopObserver) SessionOpened(types.UID)         {}
func (nopObserver) SessionClosed(types.UID)         {}
func (nopObserver) SessionFailed(types.UID, string) {}

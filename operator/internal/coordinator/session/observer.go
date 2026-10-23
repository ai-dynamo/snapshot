// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// Operation names one session operation, and is a label a metric may group by.
type Operation string

// The operations an Observer sees. A call that blocks reports its own timing
// instead, through KeyWaited or BarrierWaited, because a wait is a rendezvous
// rather than a latency and would swamp the latencies it was mixed with.
const (
	OpInsert         Operation = "insert"
	OpGet            Operation = "get"
	OpUnset          Operation = "unset"
	OpIncrement      Operation = "increment"
	OpDecrement      Operation = "decrement"
	OpCompareAndSwap Operation = "compare_and_swap"
	OpCreateBarrier  Operation = "create_barrier"
	OpPostBarrier    Operation = "post_barrier"
)

// Observer receives what sessions do, for metrics. It is never given a key, a
// barrier name, or a participant: library callers choose those, and as metric
// labels they would be unbounded. A role and an Operation are bounded.
//
// A session calls an Observer from the goroutine that ran the operation or, at
// the deadline, from the clock's callback, and never while holding a lock of
// its own. An implementation must be safe for concurrent use, must not block,
// and must not call back into the session, its Manager, or its clock.
type Observer interface {
	// SessionOpened reports a session created for an attempt.
	SessionOpened(uid types.UID)

	// SessionClosed reports a session that ended because every member left.
	SessionClosed(uid types.UID)

	// SessionFailed reports a session that ended with a reason.
	SessionFailed(uid types.UID, reason Reason)

	// OperationLatency reports how long one operation took, whether or not it
	// succeeded. A Get with a wait reports KeyWaited instead.
	OperationLatency(uid types.UID, role Role, op Operation, took time.Duration)

	// KeyWaited reports how long one Get with a wait waited, whether or not
	// its key appeared.
	KeyWaited(uid types.UID, role Role, took time.Duration)

	// BarrierWaited reports how long one caller waited on a barrier.
	BarrierWaited(uid types.UID, role Role, took time.Duration)
}

// nopObserver is the default: a Manager with no Observer configured reports
// nothing. Metrics arrive with the service that serves sessions.
type nopObserver struct{}

func (nopObserver) SessionOpened(types.UID)                                    {}
func (nopObserver) SessionClosed(types.UID)                                    {}
func (nopObserver) SessionFailed(types.UID, Reason)                            {}
func (nopObserver) OperationLatency(types.UID, Role, Operation, time.Duration) {}
func (nopObserver) KeyWaited(types.UID, Role, time.Duration)                   {}
func (nopObserver) BarrierWaited(types.UID, Role, time.Duration)               {}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import "errors"

// CheckpointPrepareRetryable reports staging failures that cannot have
// mutated the source workload. Transport ambiguity is safe here because
// PrepareCheckpoint only creates PageBroker-owned staging state; a fresh
// transaction cannot replay CUDA or CRIU work.
func CheckpointPrepareRetryable(err error) bool {
	if isTransportError(err) {
		return true
	}
	var failure failureError
	if !errors.As(err, &failure) {
		return false
	}
	switch failure.code {
	case Failure_BUSY, Failure_INSUFFICIENT_STORAGE, Failure_STORAGE_ERROR:
		return true
	default:
		return false
	}
}

// CheckpointAdmissionRetryable reports non-mutating admission outcomes that
// controller recovery can resolve by aborting the exact durably recorded
// transaction before allocating a fresh identity.
func CheckpointAdmissionRetryable(err error) bool {
	if isTransportError(err) {
		return true
	}
	var failure failureError
	if !errors.As(err, &failure) {
		return false
	}
	return failure.code == Failure_BUSY && failure.targetMutationKnown &&
		!failure.targetMayBeMutated
}

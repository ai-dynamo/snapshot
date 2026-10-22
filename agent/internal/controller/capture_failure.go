// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"

	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
)

// Reasons recorded on a work order whose dump failed. They separate the
// conditions an operator acts on differently: waiting for node staging to free
// up, fixing a request the broker refuses, and a capture whose staged data is
// already gone.
const (
	reasonCheckpointFailed   = "CheckpointFailed"
	reasonCheckpointTimeout  = "CheckpointTimeout"
	reasonStagingUnavailable = "StagingUnavailable"
	reasonStagedDataLost     = "StagedDataLost"
	reasonBrokerRejected     = "BrokerRejected"
)

// captureFailure is how a failed attempt reads: the reason recorded on the work
// order, and whether another attempt could reach a different outcome.
type captureFailure struct {
	reason    string
	retryable bool
}

// classifyCaptureFailure decides both from the error alone. It cannot see which
// step failed, so it never concludes that a dump completed; a caller that knows
// the step can narrow this further.
func classifyCaptureFailure(err error) captureFailure {
	if errors.Is(err, context.DeadlineExceeded) {
		return captureFailure{reason: reasonCheckpointTimeout}
	}
	var failure *pagebroker.FailureError
	if !errors.As(err, &failure) {
		return captureFailure{reason: reasonCheckpointFailed}
	}
	switch failure.Code() {
	case pagebroker.Failure_INSUFFICIENT_STORAGE:
		// Refused before any bytes were written, so the node is momentarily full
		// rather than this image being too big for it.
		return captureFailure{reason: reasonStagingUnavailable, retryable: true}
	case pagebroker.Failure_STORAGE_ERROR, pagebroker.Failure_INTERNAL_ERROR,
		pagebroker.Failure_UNAVAILABLE, pagebroker.Failure_STORAGE_UNAVAILABLE:
		return captureFailure{reason: reasonCheckpointFailed, retryable: true}
	case pagebroker.Failure_TRANSACTION_NOT_FOUND, pagebroker.Failure_TRANSACTION_EXPIRED:
		// The broker is not holding the transaction, so whatever was staged for it
		// is already gone and no attempt can publish it.
		return captureFailure{reason: reasonStagedDataLost}
	case pagebroker.Failure_INVALID_REQUEST, pagebroker.Failure_TRANSACTION_CONFLICT,
		pagebroker.Failure_STORE_MISMATCH, pagebroker.Failure_ACCESS_DENIED,
		pagebroker.Failure_UNSUPPORTED_ARTIFACT, pagebroker.Failure_ARTIFACT_CORRUPT:
		return captureFailure{reason: reasonBrokerRejected}
	default:
		return captureFailure{reason: reasonCheckpointFailed}
	}
}

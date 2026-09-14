// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCheckpointPrepareRetryableIsLimitedToStagingFailures(t *testing.T) {
	assert.True(t, CheckpointPrepareRetryable(transportError{cause: errors.New("lost reply"), mayHaveDispatched: true}))
	assert.True(t, CheckpointPrepareRetryable(failureError{code: Failure_BUSY}))
	assert.True(t, CheckpointPrepareRetryable(failureError{code: Failure_INSUFFICIENT_STORAGE}))
	assert.True(t, CheckpointPrepareRetryable(failureError{code: Failure_STORAGE_ERROR}))
	assert.False(t, CheckpointPrepareRetryable(failureError{code: Failure_INVALID_REQUEST}))
	assert.False(t, CheckpointPrepareRetryable(cudaResponseError{cause: errors.New("malformed response")}))
}

func TestCheckpointAdmissionRetryableIsNonMutating(t *testing.T) {
	assert.True(t, CheckpointAdmissionRetryable(transportError{
		cause: errors.New("lost admission reply"), mayHaveDispatched: true,
	}))
	assert.True(t, CheckpointAdmissionRetryable(failureError{
		code: Failure_BUSY, targetMutationKnown: true,
	}))
	assert.False(t, CheckpointAdmissionRetryable(failureError{code: Failure_BUSY}))
	assert.False(t, CheckpointAdmissionRetryable(failureError{
		code: Failure_BUSY, targetMutationKnown: true, targetMayBeMutated: true,
	}))
	assert.False(t, CheckpointAdmissionRetryable(cudaResponseError{cause: errors.New("mismatched IDs")}))
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"time"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// loadAttempts returns the destination's attempt record, and false when it
// has none yet.
func (op *restoreOperation) loadAttempts() (*podcontract.ContainerRetryStatus, bool) {
	value, found := op.controller.restoreAttemptRecords.Load(restoreContainerKey(op.pod.UID, op.destination))
	if !found {
		return nil, false
	}
	record := value.(podcontract.ContainerRetryStatus)
	return &record, true
}

// storeAttempts keeps the destination's record.
func (op *restoreOperation) storeAttempts(record *podcontract.ContainerRetryStatus) {
	op.controller.restoreAttemptRecords.Store(restoreContainerKey(op.pod.UID, op.destination), *record)
}

// startAttempt resolves the attempt about to start. A limit of 0 keeps no
// record, so every pass starts the restore over with a full timeout, as
// before retries. A restore that allows retries continues the destination's
// record, which keeps its limit and deadline, or saves the first attempt as a
// new record before it runs.
func (op *restoreOperation) startAttempt() (*podcontract.ContainerRetryStatus, error) {
	now := op.controller.nowFn()
	limit, err := op.retryLimit()
	if err != nil {
		return nil, err
	}
	if limit == 0 {
		record := op.firstAttempt(limit, now)
		op.logAttempt(record, now)
		return record, nil
	}
	record, found := op.loadAttempts()
	if !found {
		record = op.firstAttempt(limit, now)
	} else if !record.Deadline.IsZero() && !now.Before(record.Deadline) {
		return nil, fmt.Errorf("restore timed out at %s, before attempt %d could start",
			record.Deadline.UTC().Format(time.RFC3339), record.Attempt)
	}
	record.ContainerID = op.containerID
	op.storeAttempts(record)
	op.logAttempt(record, now)
	return record, nil
}

// firstAttempt is attempt 1 of a restore, with the limit and the deadline
// that ends every attempt.
func (op *restoreOperation) firstAttempt(limit int, now time.Time) *podcontract.ContainerRetryStatus {
	record := &podcontract.ContainerRetryStatus{
		Attempt:     1,
		RetryLimit:  limit,
		ContainerID: op.containerID,
		PodUID:      op.pod.UID,
	}
	if timeout := op.controller.config.Restore.RestoreTimeout(); timeout > 0 {
		record.Deadline = now.Add(timeout)
	}
	return record
}

// logAttempt names the attempt in the operation's log and logs its start.
func (op *restoreOperation) logAttempt(record *podcontract.ContainerRetryStatus, now time.Time) {
	op.log = op.log.WithValues("attempt", record.Attempt, "retry_limit", record.RetryLimit)
	if record.Deadline.IsZero() {
		op.log.Info("Starting restore attempt")
		return
	}
	op.log.Info("Starting restore attempt", "time_left", record.Deadline.Sub(now).Round(time.Second).String())
}

// forgetAttempts removes the destination's record after the gates refused
// the restore. A refusal is not an attempt, so the skip request that reopens
// the restore starts it over with a full timeout.
func (op *restoreOperation) forgetAttempts() {
	op.controller.restoreAttemptRecords.Delete(restoreContainerKey(op.pod.UID, op.destination))
}

// retryLimit is the Pod's nvidia.com/restore-retry-limit, or the agent's
// restore.retryLimit when the Pod sets none.
func (op *restoreOperation) retryLimit() (int, error) {
	limit, found, err := podcontract.RestoreRetryLimitFromAnnotations(op.pod.Annotations)
	if err != nil || found {
		return limit, err
	}
	return op.controller.config.Restore.ResolveRetryLimit(), nil
}

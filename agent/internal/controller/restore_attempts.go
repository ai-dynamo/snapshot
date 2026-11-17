// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"time"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// restoreAttempts is a destination container's record of its restore
// attempts. A restore that allows retries keeps it in the agent's memory. It
// goes with the Pod, so a new Pod starts a new restore.
type restoreAttempts struct {
	// Limit is the retry limit, resolved at the first attempt.
	Limit int
	// Attempt is the current or last attempt, from 1.
	Attempt int
	// Deadline is set at the first attempt and ends every attempt. Zero means
	// the restore has no timeout.
	Deadline time.Time
	// ContainerID is the container the attempt ran in.
	ContainerID string
}

// loadAttempts returns the destination's attempt record, and false when it
// has none yet.
func (op *restoreOperation) loadAttempts() (*restoreAttempts, bool) {
	value, found := op.controller.restoreAttemptRecords.Load(restoreContainerKey(op.pod.UID, op.destination))
	if !found {
		return nil, false
	}
	record := value.(restoreAttempts)
	return &record, true
}

// saveAttempts keeps the destination's record.
func (op *restoreOperation) saveAttempts(record *restoreAttempts) {
	op.controller.restoreAttemptRecords.Store(restoreContainerKey(op.pod.UID, op.destination), *record)
}

// startAttempt resolves the attempt about to start. A restore that allows
// retries saves it in the destination's record first. A limit of 0 keeps no
// record, so every pass starts the restore over with a full timeout, as
// before retries.
func (op *restoreOperation) startAttempt() (*restoreAttempts, error) {
	now := op.controller.nowFn()
	limit, err := op.retryLimit()
	if err != nil {
		return nil, err
	}
	record := &restoreAttempts{Limit: limit, Attempt: 1, ContainerID: op.containerID}
	if timeout := op.controller.config.Restore.RestoreTimeout(); timeout > 0 {
		record.Deadline = now.Add(timeout)
	}
	if limit > 0 {
		if record, err = op.saveAttempt(record, now); err != nil {
			return nil, err
		}
	}
	op.log = op.log.WithValues("attempt", record.Attempt, "retry_limit", record.Limit)
	if record.Deadline.IsZero() {
		op.log.Info("Starting restore attempt")
	} else {
		op.log.Info("Starting restore attempt", "time_left", record.Deadline.Sub(now).Round(time.Second).String())
	}
	return record, nil
}

// saveAttempt saves the attempt about to start in the destination's record.
// With no record yet it saves first, which holds the limit and deadline
// resolved now; a record that exists keeps its limit and deadline.
func (op *restoreOperation) saveAttempt(first *restoreAttempts, now time.Time) (*restoreAttempts, error) {
	record, found := op.loadAttempts()
	if !found {
		record = first
	} else if !record.Deadline.IsZero() && !now.Before(record.Deadline) {
		return nil, fmt.Errorf("restore timed out at %s, before attempt %d could start",
			record.Deadline.UTC().Format(time.RFC3339), record.Attempt)
	}
	record.ContainerID = op.containerID
	op.saveAttempts(record)
	return record, nil
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

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// attemptSeed copies a Pod's retry status annotation into memory once, before
// the agent loads any record of the Pod.
type attemptSeed struct {
	once sync.Once
	err  error
}

// seedAttempts copies every entry the Pod's retry status annotation holds for
// this Pod into memory, the first time the agent loads a record of the Pod, as
// after an agent restart. Every entry is copied, so the agent's next write of
// the annotation keeps the entries of destinations it has not restored since.
// An entry written for another Pod, such as one copied with the Pod's
// annotations, is skipped. An annotation that cannot be parsed is an error:
// the restore ends rather than grant attempts it cannot count.
func (w *NodeController) seedAttempts(pod *corev1.Pod) error {
	value, _ := w.attemptSeeds.LoadOrStore(pod.UID, &attemptSeed{})
	seed := value.(*attemptSeed)
	seed.once.Do(func() {
		status, found, err := podcontract.RestoreRetryStatusFromAnnotations(pod.Annotations)
		if err != nil {
			seed.err = err
			return
		}
		if found {
			// Counted as written, so a write that leaves no records still removes it.
			w.retryStatusWritten.LoadOrStore(pod.UID, pod.Annotations[podcontract.RestoreRetryStatusAnnotation])
		}
		for container, entry := range status {
			if entry.PodUID != pod.UID {
				continue
			}
			if entry.ContainerID == "" {
				seed.err = fmt.Errorf("%s entry for container %q has no container ID",
					podcontract.RestoreRetryStatusAnnotation, container)
				return
			}
			w.restoreAttemptRecords.LoadOrStore(restoreContainerKey(pod.UID, container), entry)
		}
	})
	return seed.err
}

// loadAttempts returns the destination's attempt record, and false when it
// has none yet.
func (op *restoreOperation) loadAttempts() (*podcontract.ContainerRetryStatus, bool, error) {
	if err := op.controller.seedAttempts(op.pod); err != nil {
		return nil, false, err
	}
	value, found := op.controller.restoreAttemptRecords.Load(restoreContainerKey(op.pod.UID, op.destination))
	if !found {
		return nil, false, nil
	}
	record := value.(podcontract.ContainerRetryStatus)
	return &record, true, nil
}

// storeAttempts keeps the destination's record and queues the Pod's retry
// status annotation for a write, which runs outside the restore.
func (op *restoreOperation) storeAttempts(record *podcontract.ContainerRetryStatus) {
	op.controller.restoreAttemptRecords.Store(restoreContainerKey(op.pod.UID, op.destination), *record)
	op.controller.queueRetryStatus(op.pod)
}

// startAttempt resolves the attempt about to start. A limit of 0 keeps no
// record, so a restarted agent starts the restore over with a full timeout,
// as before retries. A restore that allows retries saves the first attempt as
// a new record before it runs; an agent that restarted during an attempt
// repeats it within the record's limit and deadline.
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
	record, found, err := op.loadAttempts()
	if err != nil {
		return nil, err
	}
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
	op.controller.queueRetryStatus(op.pod)
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

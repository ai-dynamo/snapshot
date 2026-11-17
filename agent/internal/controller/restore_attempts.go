// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	ktypes "k8s.io/apimachinery/pkg/types"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// restoreAttempts is a destination container's record of its restore
// attempts. A restore that allows retries keeps it in the agent's memory and
// copies it to the Pod's retry status annotation, which a restarted agent
// reads it back from. It goes with the Pod, so a new Pod starts a new restore.
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
	// RetryAt is when an in-place retry falls due, and zero otherwise.
	RetryAt time.Time
}

func attemptsFromStatus(entry podcontract.ContainerRetryStatus) restoreAttempts {
	return restoreAttempts{
		Limit:       entry.RetryLimit,
		Attempt:     entry.Attempt,
		Deadline:    entry.Deadline,
		ContainerID: entry.ContainerID,
		RetryAt:     entry.RetryAt,
	}
}

func (r restoreAttempts) status(podUID ktypes.UID) podcontract.ContainerRetryStatus {
	return podcontract.ContainerRetryStatus{
		Attempt:     r.Attempt,
		RetryLimit:  r.Limit,
		Deadline:    r.Deadline,
		ContainerID: r.ContainerID,
		RetryAt:     r.RetryAt,
		PodUID:      podUID,
	}
}

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
			w.restoreAttemptRecords.LoadOrStore(restoreContainerKey(pod.UID, container), attemptsFromStatus(entry))
		}
	})
	return seed.err
}

// loadAttempts returns the destination's attempt record, and false when it
// has none yet.
func (op *restoreOperation) loadAttempts() (*restoreAttempts, bool, error) {
	if err := op.controller.seedAttempts(op.pod); err != nil {
		return nil, false, err
	}
	value, found := op.controller.restoreAttemptRecords.Load(restoreContainerKey(op.pod.UID, op.destination))
	if !found {
		return nil, false, nil
	}
	record := value.(restoreAttempts)
	return &record, true, nil
}

// saveAttempts keeps the destination's record and queues the Pod's retry
// status annotation for a write, which runs outside the restore.
func (op *restoreOperation) saveAttempts(record *restoreAttempts) {
	op.controller.restoreAttemptRecords.Store(restoreContainerKey(op.pod.UID, op.destination), *record)
	op.controller.queueRetryStatus(op.pod)
}

// startAttempt resolves the attempt about to start. A restore that allows
// retries picks it up from the destination's record and saves it there first,
// and gets errRetryScheduled while an in-place retry is not due. A limit of 0
// keeps no record, so a restarted agent starts the restore over with a full
// timeout, as before retries.
func (op *restoreOperation) startAttempt(ctx context.Context) (*restoreAttempts, error) {
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
		if record, err = op.saveAttempt(ctx, record, now); err != nil {
			return nil, err
		}
	}
	op.log = op.log.WithValues("attempt", record.Attempt, "retry_limit", record.Limit)
	return record, nil
}

// saveAttempt picks the restore up from the destination's record and saves
// the attempt about to start. With no record yet it saves first, which holds
// the limit and deadline resolved now.
//
// A record with no retry saved holds an attempt that no agent saw finish,
// since one agent never runs two passes of a Pod at once. The attempt runs
// again within the same deadline and keeps its number, as an agent restart
// always repeated it.
func (op *restoreOperation) saveAttempt(ctx context.Context, first *restoreAttempts, now time.Time) (*restoreAttempts, error) {
	record, found, err := op.loadAttempts()
	if err != nil {
		return nil, err
	}
	if !found {
		record = first
	} else {
		if now.Before(record.RetryAt) {
			op.log.V(1).Info("Restore retry is not due yet", "retry_at", record.RetryAt.UTC().Format(time.RFC3339))
			return nil, errRetryScheduled
		}
		next := record.Attempt
		if !record.RetryAt.IsZero() {
			next++
		}
		if !record.Deadline.IsZero() && !now.Before(record.Deadline) {
			return nil, op.retriesExhausted(ctx, record, true, fmt.Errorf("restore timed out at %s, before attempt %d could start",
				record.Deadline.UTC().Format(time.RFC3339), next))
		}
		if next > record.Limit+1 {
			return nil, op.retriesExhausted(ctx, record, false, fmt.Errorf("attempt %d is over the limit of %d", next, record.Limit+1))
		}
		record.Attempt = next
		record.RetryAt = time.Time{}
	}
	record.ContainerID = op.containerID
	op.saveAttempts(record)
	return record, nil
}

// announceAttempt emits RestoreRequested for every attempt that starts. A
// restore that allows retries names the attempt in it.
func (op *restoreOperation) announceAttempt(ctx context.Context, record *restoreAttempts) {
	keysAndValues := []any{
		"content_uid", op.artifact.ContentUID,
		"source_container", op.artifact.SourceContainerName,
		"container_id", op.containerID,
	}
	if !record.Deadline.IsZero() {
		keysAndValues = append(keysAndValues, "time_left", record.Deadline.Sub(op.controller.nowFn()).Round(time.Second).String())
	}
	op.log.Info("Restore target detected, triggering external restore", keysAndValues...)
	message := fmt.Sprintf("Restore requested from PodSnapshot %s for destination %s", op.artifact.SnapshotName, op.destination)
	if record.Limit > 0 {
		message += fmt.Sprintf(restoreRequestedAttemptSuffix, record.Attempt, record.Limit+1)
	}
	emitPodEvent(ctx, op.controller.clientset, op.log, op.pod, snapshotEventComponent, corev1.EventTypeNormal, restoreRequestedReason, message)
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

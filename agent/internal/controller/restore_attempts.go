// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// restoreAttemptsMaxBytes bounds the read of a record the Pod can rewrite.
// It holds a LastFailure whose cause JSON escapes to six times its size.
const restoreAttemptsMaxBytes = 8 << 10

// restoreAttempts is a destination container's record of its restore
// attempts, kept in its directory of the Pod's snapshot-control volume. The
// volume outlives container restarts and goes with the Pod, so a new Pod
// starts a new restore.
type restoreAttempts struct {
	// Limit is the retry limit, resolved at the first attempt.
	Limit int `json:"limit"`
	// Attempt is the current or last attempt, from 1.
	Attempt int `json:"attempt"`
	// Deadline is set at the first attempt and ends every attempt. Zero means
	// the restore has no timeout.
	Deadline time.Time `json:"deadline"`
	// ContainerID is the container the attempt ran in.
	ContainerID string `json:"containerID"`
	// RetryAt is when an in-place retry falls due, and zero otherwise.
	RetryAt time.Time `json:"retryAt"`
	// LastFailure is the last failed attempt, the source of the retry status
	// annotation.
	LastFailure *podcontract.FailureSummary `json:"lastFailure,omitempty"`
}

func parseRestoreAttempts(data []byte) (*restoreAttempts, error) {
	var record restoreAttempts
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, err
	}
	switch {
	case record.Attempt < 1:
		return nil, fmt.Errorf("attempt %d is not positive", record.Attempt)
	case record.Limit < 0:
		return nil, fmt.Errorf("retry limit %d is negative", record.Limit)
	case record.ContainerID == "":
		return nil, errors.New("no container ID")
	}
	return &record, nil
}

// readRestoreAttempts returns the destination's attempt record, and false when
// it has none yet. A record that cannot be read or parsed is an error: the
// restore ends rather than grant attempts it cannot count.
func (op *restoreOperation) readRestoreAttempts(nodeUID string) (*restoreAttempts, bool, error) {
	return op.controller.readRestoreAttempts(nodeUID, op.destination)
}

func (w *NodeController) readRestoreAttempts(nodeUID, destination string) (*restoreAttempts, bool, error) {
	data, err := w.readPodControlFileFn(nodeUID, destination, podcontract.RestoreAttemptsFile, restoreAttemptsMaxBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read restore attempt record: %w", err)
	}
	record, err := parseRestoreAttempts(data)
	if err != nil {
		return nil, false, fmt.Errorf("restore attempt record %q is invalid: %w", podcontract.RestoreAttemptsFile, err)
	}
	return record, true, nil
}

func (op *restoreOperation) writeRestoreAttempts(nodeUID string, record *restoreAttempts) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(data) > restoreAttemptsMaxBytes {
		return fmt.Errorf("save restore attempt record: %d bytes is more than the %d a pass reads", len(data), restoreAttemptsMaxBytes)
	}
	if err := op.controller.writePodControlSentinelFn(nodeUID, op.destination, podcontract.RestoreAttemptsFile, data); err != nil {
		return fmt.Errorf("save restore attempt record: %w", err)
	}
	return nil
}

// startAttempt picks the restore up from the destination's record and saves
// the attempt about to start. It returns errRetryScheduled while an in-place
// retry is not due. The first attempt resolves the retry limit and the
// deadline.
//
// A record with no retry saved holds an attempt that no agent saw finish,
// since one agent never runs two passes of a Pod at once. The attempt runs
// again within the same deadline and keeps its number, as an agent restart
// always repeated it.
func (op *restoreOperation) startAttempt(ctx context.Context) (*restoreAttempts, error) {
	now := op.controller.nowFn()
	record, found, err := op.readRestoreAttempts(op.nodeUID)
	if err != nil {
		return nil, err
	}
	if !found {
		limit, err := op.retryLimit()
		if err != nil {
			return nil, err
		}
		record = &restoreAttempts{Limit: limit, Attempt: 1}
		if timeout := op.controller.config.Restore.RestoreTimeout(); timeout > 0 {
			record.Deadline = now.Add(timeout)
		}
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
	if err := op.writeRestoreAttempts(op.nodeUID, record); err != nil {
		return nil, err
	}
	op.log = op.log.WithValues("attempt", record.Attempt, "retry_limit", record.Limit)
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
		message += fmt.Sprintf(" (attempt %d of %d)", record.Attempt, record.Limit+1)
	}
	emitPodEvent(ctx, op.controller.clientset, op.log, op.pod, snapshotEventComponent, corev1.EventTypeNormal, restoreRequestedReason, message)
}

// forgetAttempts removes the destination's record after the gates refused
// the restore. A refusal is not an attempt, so the skip request that reopens
// the restore starts it over with a full timeout.
func (op *restoreOperation) forgetAttempts(ctx context.Context) {
	if err := op.controller.removePodControlFileFn(op.nodePodUID(ctx), op.destination, podcontract.RestoreAttemptsFile); err != nil {
		op.log.Error(err, "Failed to remove the restore attempt record after the refusal")
	}
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

// nodePodUID is the Pod's UID on the node, which names the Pod's directories
// under kubelet's. In a vCluster it differs from the Pod's own UID; the
// runtime knows it while the container exists.
func (op *restoreOperation) nodePodUID(ctx context.Context) string {
	nodeUID, err := op.controller.runtime.ResolveContainerPodUID(ctx, op.containerID)
	if err != nil {
		op.log.V(1).Info("Cannot read the Pod's UID on the node; using its own", "error", err.Error())
		return string(op.pod.UID)
	}
	return nodeUID
}

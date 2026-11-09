// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// RestoreOutcome is the stable, consumer-facing state of a restore request.
type RestoreOutcome string

const (
	// RestoreOutcomeUnknown means the Restored condition exists but cannot be
	// classified safely by this API version.
	RestoreOutcomeUnknown RestoreOutcome = "Unknown"
	// RestoreOutcomePending means restore has not reached a terminal outcome.
	// It includes an absent condition and active restore execution.
	RestoreOutcomePending RestoreOutcome = "Pending"
	// RestoreOutcomeSucceeded means every requested destination was restored.
	RestoreOutcomeSucceeded RestoreOutcome = "Succeeded"
	// RestoreOutcomeFailed means no requested destination was restored.
	RestoreOutcomeFailed RestoreOutcome = "Failed"
	// RestoreOutcomePartiallySucceeded means some, but not all, requested
	// destinations were restored.
	RestoreOutcomePartiallySucceeded RestoreOutcome = "PartiallySucceeded"
)

// Terminal reports whether the restore outcome is final.
func (o RestoreOutcome) Terminal() bool {
	switch o {
	case RestoreOutcomeSucceeded, RestoreOutcomeFailed, RestoreOutcomePartiallySucceeded:
		return true
	default:
		return false
	}
}

// Stable reasons used on the nvidia.com/Restored Pod condition. Dependency-wait
// reasons remain agent-internal; consumers should use ClassifyRestoreOutcome.
const (
	// RestoredCondition is the Pod status condition owned by the Snapshot node
	// agent.
	RestoredCondition = "nvidia.com/Restored"

	// RestoreReasonInProgress marks active restore execution.
	RestoreReasonInProgress = "RestoreInProgress"
	// RestoreReasonSucceeded marks a terminal all-destinations success.
	RestoreReasonSucceeded = "RestoreSucceeded"
	// RestoreReasonFailed marks a terminal all-destinations failure.
	RestoreReasonFailed = "RestoreFailed"
	// RestoreReasonPartiallySucceeded marks a terminal mixed destination outcome.
	RestoreReasonPartiallySucceeded = "RestorePartiallySucceeded"
	// RestoreReasonIncompatible marks a restore refused before CRIU.
	RestoreReasonIncompatible = "RestoreIncompatible"
)

// ClassifyRestoreOutcome returns the public restore outcome represented by Pod
// conditions. A missing Restored condition is Pending. An unrecognized status
// or reason is Unknown so callers can apply their own version-skew policy.
func ClassifyRestoreOutcome(conditions []corev1.PodCondition) RestoreOutcome {
	for _, condition := range conditions {
		if condition.Type != corev1.PodConditionType(RestoredCondition) {
			continue
		}
		if condition.Status == corev1.ConditionTrue {
			return RestoreOutcomeSucceeded
		}
		if condition.Status != corev1.ConditionFalse {
			return RestoreOutcomeUnknown
		}
		switch condition.Reason {
		case RestoreReasonFailed, RestoreReasonIncompatible:
			return RestoreOutcomeFailed
		case RestoreReasonPartiallySucceeded:
			return RestoreOutcomePartiallySucceeded
		case RestoreReasonInProgress:
			return RestoreOutcomePending
		default:
			return RestoreOutcomeUnknown
		}
	}
	return RestoreOutcomePending
}

// RestoreRetryStatusAnnotation is written by the node agent on a restore Pod
// once a destination container's restore attempt fails. Its value is a JSON
// RestoreRetryStatus. The agent never reads it back.
const RestoreRetryStatusAnnotation = "nvidia.com/restore-retry-status"

// FailureCauseMaxBytes bounds FailureSummary.Cause, the limit of an event
// message.
const FailureCauseMaxBytes = 1024

// RestoreRetryStatus holds one entry per destination container with a failed
// attempt, keyed by container name.
type RestoreRetryStatus map[string]ContainerRetryStatus

// ContainerRetryStatus is one destination container's attempts.
type ContainerRetryStatus struct {
	// Attempt is the current or last attempt, from 1.
	Attempt int `json:"attempt"`
	// RetryLimit is the limit resolved at the first attempt.
	RetryLimit int `json:"retryLimit"`
	// LastFailure is the last failed attempt.
	LastFailure FailureSummary `json:"lastFailure"`
}

// FailureSummary says where and why a restore attempt failed.
type FailureSummary struct {
	// Step is where the attempt failed.
	Step string `json:"step"`
	// Cause is the failure, cut to FailureCauseMaxBytes.
	Cause string `json:"cause"`
	// SnapshotUnusable is set when the snapshot itself is missing, corrupt,
	// or unsupported, so no restore from it can succeed.
	SnapshotUnusable bool `json:"snapshotUnusable,omitempty"`
}

// RestoreRetryStatusFromAnnotations returns the retry status the node agent
// wrote on a restore Pod. found is false when the annotation is absent.
func RestoreRetryStatusFromAnnotations(
	annotations map[string]string,
) (status RestoreRetryStatus, found bool, err error) {
	raw, found := annotations[RestoreRetryStatusAnnotation]
	if !found {
		return nil, false, nil
	}
	if err := json.Unmarshal([]byte(raw), &status); err != nil {
		return nil, true, fmt.Errorf("parse %s: %w", RestoreRetryStatusAnnotation, err)
	}
	for container, entry := range status {
		if entry.Attempt < 1 || entry.RetryLimit < 0 {
			return nil, true, fmt.Errorf(
				"%s entry for container %q has attempt %d and retry limit %d",
				RestoreRetryStatusAnnotation, container, entry.Attempt, entry.RetryLimit,
			)
		}
	}
	return status, true, nil
}

// FormatRestoreRetryStatus renders the value of RestoreRetryStatusAnnotation.
// Equal statuses render to equal strings.
func FormatRestoreRetryStatus(status RestoreRetryStatus) (string, error) {
	data, err := json.Marshal(status)
	if err != nil {
		return "", fmt.Errorf("format %s: %w", RestoreRetryStatusAnnotation, err)
	}
	return string(data), nil
}

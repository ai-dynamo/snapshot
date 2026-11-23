// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// RestoreRetryLimitAnnotation overrides the node agent's restore.retryLimit
// for one restore Pod: how many more attempts a destination container gets
// after its first restore attempt fails. Its value is a non-negative
// integer, and 0 means one attempt.
const RestoreRetryLimitAnnotation = "nvidia.com/restore-retry-limit"

// RestoreRetryLimitFromAnnotations returns the retry limit a restore Pod sets.
// found is false when the annotation is absent, so the caller applies its
// default. A value that is not a non-negative integer is an error.
func RestoreRetryLimitFromAnnotations(annotations map[string]string) (limit int, found bool, err error) {
	raw, found := annotations[RestoreRetryLimitAnnotation]
	if !found {
		return 0, false, nil
	}
	limit, err = parseRestoreRetryLimit(raw)
	if err != nil {
		return 0, true, err
	}
	return limit, true, nil
}

func parseRestoreRetryLimit(raw string) (int, error) {
	limit, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 31)
	if err != nil {
		return 0, fmt.Errorf("%s value %q must be a non-negative integer: %w", RestoreRetryLimitAnnotation, raw, err)
	}
	return int(limit), nil
}

// RestoreRetryStatusAnnotation is written by the node agent on a restore Pod
// when a destination container's restore attempt starts and when it fails,
// once the restore has a nonzero retry limit. Its value is a JSON
// RestoreRetryStatus. A restarted agent reads it back to continue the
// restore's attempts.
const RestoreRetryStatusAnnotation = "nvidia.com/restore-retry-status"

// FailureCauseMaxBytes bounds FailureSummary.Cause, the limit of an event
// message.
const FailureCauseMaxBytes = 1024

// RestoreRetryStatus holds one entry per destination container that started
// an attempt, keyed by container name. Decode it with
// RestoreRetryStatusFromAnnotations, which checks each entry.
type RestoreRetryStatus map[string]ContainerRetryStatus

// ContainerRetryStatus is one destination container's attempts.
type ContainerRetryStatus struct {
	// Attempt is the current or last attempt, from 1.
	Attempt int `json:"attempt"`
	// RetryLimit is the limit resolved at the first attempt.
	RetryLimit int `json:"retryLimit"`
	// LastFailure is the last failed attempt, absent until one fails.
	LastFailure *FailureSummary `json:"lastFailure,omitempty"`
	// RetryAt is when the next attempt is due, set while an in-place retry
	// waits.
	RetryAt time.Time `json:"retryAt,omitzero"`
	// Deadline ends every attempt, set at the first attempt.
	Deadline time.Time `json:"deadline,omitzero"`
	// ContainerID is the container the attempt ran in.
	ContainerID string `json:"containerID,omitempty"`
	// PodUID is the Pod the entry was written for, so an entry copied to
	// another Pod is not mistaken for that Pod's own.
	PodUID types.UID `json:"podUID,omitempty"`
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

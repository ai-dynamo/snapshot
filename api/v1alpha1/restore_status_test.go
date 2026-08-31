// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestClassifyRestoreOutcome(t *testing.T) {
	restored := func(status corev1.ConditionStatus, reason string) corev1.PodCondition {
		return corev1.PodCondition{
			Type:   corev1.PodConditionType(RestoredCondition),
			Status: status,
			Reason: reason,
		}
	}
	tests := []struct {
		name       string
		conditions []corev1.PodCondition
		want       RestoreOutcome
	}{
		{name: "condition absent", want: RestoreOutcomePending},
		{
			name: "unrelated condition",
			conditions: []corev1.PodCondition{{
				Type: corev1.PodReady, Status: corev1.ConditionTrue,
			}},
			want: RestoreOutcomePending,
		},
		{
			name:       "unrecognized dependency reason",
			conditions: []corev1.PodCondition{restored(corev1.ConditionFalse, "SnapshotPending")},
			want:       RestoreOutcomeUnknown,
		},
		{
			name:       "unrecognized terminal reason",
			conditions: []corev1.PodCondition{restored(corev1.ConditionFalse, "RestoreIncompatible")},
			want:       RestoreOutcomeUnknown,
		},
		{
			name:       "restore in progress",
			conditions: []corev1.PodCondition{restored(corev1.ConditionFalse, RestoreReasonInProgress)},
			want:       RestoreOutcomePending,
		},
		{
			name:       "unknown status",
			conditions: []corev1.PodCondition{restored(corev1.ConditionUnknown, RestoreReasonSucceeded)},
			want:       RestoreOutcomeUnknown,
		},
		{
			name:       "succeeded",
			conditions: []corev1.PodCondition{restored(corev1.ConditionTrue, RestoreReasonSucceeded)},
			want:       RestoreOutcomeSucceeded,
		},
		{
			name:       "succeeded with omitted reason",
			conditions: []corev1.PodCondition{restored(corev1.ConditionTrue, "")},
			want:       RestoreOutcomeSucceeded,
		},
		{
			name:       "failed",
			conditions: []corev1.PodCondition{restored(corev1.ConditionFalse, RestoreReasonFailed)},
			want:       RestoreOutcomeFailed,
		},
		{
			name:       "partially succeeded",
			conditions: []corev1.PodCondition{restored(corev1.ConditionFalse, RestoreReasonPartiallySucceeded)},
			want:       RestoreOutcomePartiallySucceeded,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ClassifyRestoreOutcome(test.conditions); got != test.want {
				t.Fatalf("ClassifyRestoreOutcome() = %q, want %q", got, test.want)
			}
		})
	}
}

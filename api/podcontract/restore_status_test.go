// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
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
			name:       "incompatible",
			conditions: []corev1.PodCondition{restored(corev1.ConditionFalse, RestoreReasonIncompatible)},
			want:       RestoreOutcomeFailed,
		},
		{
			name:       "unrecognized terminal reason",
			conditions: []corev1.PodCondition{restored(corev1.ConditionFalse, "FutureTerminalReason")},
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

func TestRestoreRetryStatusRoundTrip(t *testing.T) {
	deadline := time.Date(2026, time.November, 17, 12, 0, 0, 0, time.UTC)
	status := RestoreRetryStatus{
		"worker": {
			Attempt:    2,
			RetryLimit: 3,
			LastFailure: &FailureSummary{
				Step:  "preparing PageBroker restore",
				Cause: "PageBroker UNAVAILABLE: too many GPU connections",
			},
			RetryAt:     deadline.Add(-time.Hour),
			Deadline:    deadline,
			ContainerID: "containerd://worker",
			PodUID:      "pod-uid",
		},
		"sidecar": {
			Attempt:     1,
			RetryLimit:  0,
			LastFailure: &FailureSummary{Step: "staging artifact", Cause: "corrupt", SnapshotUnusable: true},
		},
		"loader": {Attempt: 1, RetryLimit: 2, Deadline: deadline, ContainerID: "containerd://loader", PodUID: "pod-uid"},
	}
	value, err := FormatRestoreRetryStatus(status)
	if err != nil {
		t.Fatalf("FormatRestoreRetryStatus() failed: %v", err)
	}
	const want = `{"loader":{"attempt":1,"retryLimit":2,"deadline":"2026-11-17T12:00:00Z",` +
		`"containerID":"containerd://loader","podUID":"pod-uid"},` +
		`"sidecar":{"attempt":1,"retryLimit":0,` +
		`"lastFailure":{"step":"staging artifact","cause":"corrupt","snapshotUnusable":true}},` +
		`"worker":{"attempt":2,"retryLimit":3,` +
		`"lastFailure":{"step":"preparing PageBroker restore","cause":"PageBroker UNAVAILABLE: too many GPU connections"},` +
		`"retryAt":"2026-11-17T11:00:00Z","deadline":"2026-11-17T12:00:00Z",` +
		`"containerID":"containerd://worker","podUID":"pod-uid"}}`
	if value != want {
		t.Fatalf("FormatRestoreRetryStatus() = %s\nwant %s", value, want)
	}

	parsed, found, err := RestoreRetryStatusFromAnnotations(map[string]string{RestoreRetryStatusAnnotation: value})
	if err != nil || !found {
		t.Fatalf("RestoreRetryStatusFromAnnotations() = found %v, error %v", found, err)
	}
	if !reflect.DeepEqual(parsed, status) {
		t.Fatalf("round trip = %#v, want %#v", parsed, status)
	}
}

func TestRestoreRetryStatusFromAnnotations(t *testing.T) {
	tests := []struct {
		name      string
		value     *string
		wantFound bool
		wantErr   bool
	}{
		{name: "absent"},
		{name: "empty object", value: ptr.To("{}"), wantFound: true},
		{
			name:      "unknown fields are ignored",
			value:     ptr.To(`{"main":{"attempt":1,"retryLimit":2,"lastFailure":{"step":"s","cause":"c"},"future":true}}`),
			wantFound: true,
		},
		{
			name:      "attempt that has not failed",
			value:     ptr.To(`{"main":{"attempt":1,"retryLimit":2,"deadline":"2026-11-17T12:00:00Z","containerID":"c"}}`),
			wantFound: true,
		},
		{name: "not JSON", value: ptr.To("attempt 2"), wantFound: true, wantErr: true},
		{
			name:      "deadline not a time",
			value:     ptr.To(`{"main":{"attempt":1,"retryLimit":2,"deadline":"soon"}}`),
			wantFound: true,
			wantErr:   true,
		},
		{name: "wrong shape", value: ptr.To(`["main"]`), wantFound: true, wantErr: true},
		{name: "attempt zero", value: ptr.To(`{"main":{"attempt":0,"retryLimit":2}}`), wantFound: true, wantErr: true},
		{name: "negative limit", value: ptr.To(`{"main":{"attempt":1,"retryLimit":-1}}`), wantFound: true, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			annotations := map[string]string{}
			if test.value != nil {
				annotations[RestoreRetryStatusAnnotation] = *test.value
			}
			_, found, err := RestoreRetryStatusFromAnnotations(annotations)
			if found != test.wantFound || (err != nil) != test.wantErr {
				t.Fatalf("RestoreRetryStatusFromAnnotations() = found %v, error %v, want found %v, error %v",
					found, err, test.wantFound, test.wantErr)
			}
		})
	}
}

func TestRestoreOutcomeTerminal(t *testing.T) {
	tests := map[RestoreOutcome]bool{
		RestoreOutcomeUnknown:            false,
		RestoreOutcomePending:            false,
		RestoreOutcomeSucceeded:          true,
		RestoreOutcomeFailed:             true,
		RestoreOutcomePartiallySucceeded: true,
		RestoreOutcome("future-outcome"): false,
	}
	for outcome, want := range tests {
		if got := outcome.Terminal(); got != want {
			t.Errorf("RestoreOutcome(%q).Terminal() = %t, want %t", outcome, got, want)
		}
	}
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"reflect"
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
	status := RestoreRetryStatus{
		"worker": {
			Attempt:    2,
			RetryLimit: 3,
			LastFailure: FailureSummary{
				Step:  "preparing PageBroker restore",
				Cause: "PageBroker UNAVAILABLE: too many GPU connections",
			},
		},
		"sidecar": {
			Attempt:     1,
			RetryLimit:  0,
			LastFailure: FailureSummary{Step: "staging artifact", Cause: "corrupt", SnapshotUnusable: true},
		},
	}
	value, err := FormatRestoreRetryStatus(status)
	if err != nil {
		t.Fatalf("FormatRestoreRetryStatus() failed: %v", err)
	}
	const want = `{"sidecar":{"attempt":1,"retryLimit":0,` +
		`"lastFailure":{"step":"staging artifact","cause":"corrupt","snapshotUnusable":true}},` +
		`"worker":{"attempt":2,"retryLimit":3,` +
		`"lastFailure":{"step":"preparing PageBroker restore","cause":"PageBroker UNAVAILABLE: too many GPU connections"}}}`
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
		{name: "empty object", value: ptr("{}"), wantFound: true},
		{
			name:      "unknown fields are ignored",
			value:     ptr(`{"main":{"attempt":1,"retryLimit":2,"lastFailure":{"step":"s","cause":"c"},"future":true}}`),
			wantFound: true,
		},
		{name: "not JSON", value: ptr("attempt 2"), wantFound: true, wantErr: true},
		{name: "wrong shape", value: ptr(`["main"]`), wantFound: true, wantErr: true},
		{name: "attempt zero", value: ptr(`{"main":{"attempt":0,"retryLimit":2}}`), wantFound: true, wantErr: true},
		{name: "negative limit", value: ptr(`{"main":{"attempt":1,"retryLimit":-1}}`), wantFound: true, wantErr: true},
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

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"reflect"
	"testing"
	"time"

	"k8s.io/utils/ptr"
)

func TestRestoreRetryLimitFromAnnotations(t *testing.T) {
	tests := []struct {
		name      string
		value     *string
		wantLimit int
		wantFound bool
		wantErr   bool
	}{
		{name: "absent"},
		{name: "zero", value: ptr.To("0"), wantFound: true},
		{name: "positive", value: ptr.To("5"), wantLimit: 5, wantFound: true},
		{name: "surrounding space", value: ptr.To(" 3 "), wantLimit: 3, wantFound: true},
		{name: "empty", value: ptr.To(""), wantFound: true, wantErr: true},
		{name: "negative", value: ptr.To("-1"), wantFound: true, wantErr: true},
		{name: "not a number", value: ptr.To("two"), wantFound: true, wantErr: true},
		{name: "fraction", value: ptr.To("1.5"), wantFound: true, wantErr: true},
		{name: "trailing text", value: ptr.To("2x"), wantFound: true, wantErr: true},
		{name: "overflow", value: ptr.To("99999999999999999999"), wantFound: true, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			annotations := map[string]string{}
			if test.value != nil {
				annotations[RestoreRetryLimitAnnotation] = *test.value
			}
			limit, found, err := RestoreRetryLimitFromAnnotations(annotations)
			if (err != nil) != test.wantErr {
				t.Fatalf("RestoreRetryLimitFromAnnotations() error = %v, wantErr %v", err, test.wantErr)
			}
			if limit != test.wantLimit || found != test.wantFound {
				t.Fatalf("RestoreRetryLimitFromAnnotations() = %d, %v, want %d, %v", limit, found, test.wantLimit, test.wantFound)
			}
		})
	}

	if _, found, err := RestoreRetryLimitFromAnnotations(nil); found || err != nil {
		t.Fatalf("nil annotations = found %v, error %v", found, err)
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

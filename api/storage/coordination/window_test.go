// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordination

import (
	"testing"
	"time"
)

func TestDeletionWindowValidate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		window  DeletionWindow
		wantErr bool
	}{
		{"valid", DeletionWindow{AdmissionWindow: time.Hour, ClockSkewAllowance: time.Second}, false},
		{"zero admission window", DeletionWindow{AdmissionWindow: 0}, true},
		{"negative skew", DeletionWindow{AdmissionWindow: time.Hour, ClockSkewAllowance: -time.Second}, true},
		{"skew exceeds admission window", DeletionWindow{AdmissionWindow: time.Second, ClockSkewAllowance: time.Hour}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.window.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestDeletionWindowQuiescenceDeadline(t *testing.T) {
	w := DeletionWindow{AdmissionWindow: time.Hour, ClockSkewAllowance: time.Minute}
	lastSeen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	want := lastSeen.Add(time.Hour + PageBrokerTransactionLifetime + time.Minute)
	if got := w.QuiescenceDeadline(lastSeen); !got.Equal(want) {
		t.Fatalf("QuiescenceDeadline() = %v, want %v", got, want)
	}
}

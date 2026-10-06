// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordination

import (
	"fmt"
	"time"
)

// DeletionWindow is the Stage 1 bounded-deletion configuration: admission
// window, max transaction lifetime, and clock-skew allowance.
type DeletionWindow struct {
	AdmissionWindow    time.Duration
	MaxTransactionLife time.Duration
	ClockSkewAllowance time.Duration
}

// Validate rejects a configuration that cannot safely bound deletion.
func (w DeletionWindow) Validate() error {
	if w.AdmissionWindow <= 0 {
		return fmt.Errorf("admission window must be positive")
	}
	if w.MaxTransactionLife <= 0 {
		return fmt.Errorf("max transaction lifetime must be positive")
	}
	if w.ClockSkewAllowance < 0 {
		return fmt.Errorf("clock skew allowance must not be negative")
	}
	if w.ClockSkewAllowance >= w.AdmissionWindow {
		return fmt.Errorf("clock skew allowance must be smaller than the admission window")
	}
	return nil
}

// QuiescenceDeadline is the earliest time an artifact last seen at lastSeen
// may be swept.
func (w DeletionWindow) QuiescenceDeadline(lastSeen time.Time) time.Time {
	return lastSeen.Add(w.AdmissionWindow + w.MaxTransactionLife + w.ClockSkewAllowance)
}

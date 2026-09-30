// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

func TestNormalizeProcessGroupKillErrorReportsFinishedProcess(t *testing.T) {
	if err := normalizeProcessGroupKillError(syscall.ESRCH); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("normalizeProcessGroupKillError() error = %v, want %v", err, os.ErrProcessDone)
	}
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package executor

import (
	"errors"
	"syscall"
	"testing"
)

func TestRenameNoReplaceRejectsUnsupportedPlatform(t *testing.T) {
	if err := renameNoReplace("staged", "final"); !errors.Is(err, syscall.ENOTSUP) {
		t.Fatalf("renameNoReplace error = %v, want ENOTSUP", err)
	}
}

func TestValidateCheckpointStorageRejectsUnsupportedPlatform(t *testing.T) {
	if err := ValidateCheckpointStorage(t.TempDir()); !errors.Is(err, syscall.ENOTSUP) {
		t.Fatalf("ValidateCheckpointStorage error = %v, want ENOTSUP", err)
	}
}

func TestPublishCheckpointRejectsUnsupportedPlatform(t *testing.T) {
	if err := publishCheckpoint("staged", "final", "checkpoint-123"); !errors.Is(err, syscall.ENOTSUP) {
		t.Fatalf("publishCheckpoint error = %v, want ENOTSUP", err)
	}
}

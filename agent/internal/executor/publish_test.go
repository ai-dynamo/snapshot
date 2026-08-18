// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package executor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

func TestPublishCheckpointAcceptsMatchingImmutableArtifact(t *testing.T) {
	root := t.TempDir()
	staged := filepath.Join(root, "staged")
	final := filepath.Join(root, "final")
	for _, dir := range []string{staged, final} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := types.WriteManifest(final, &types.CheckpointManifest{CheckpointID: "checkpoint-123"}); err != nil {
		t.Fatal(err)
	}

	if err := publishCheckpoint(staged, final, "checkpoint-123"); err != nil {
		t.Fatalf("publishCheckpoint: %v", err)
	}
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("staged artifact should remain for deferred cleanup: %v", err)
	}
}

func TestPublishCheckpointRejectsConflictingImmutableArtifact(t *testing.T) {
	root := t.TempDir()
	staged := filepath.Join(root, "staged")
	final := filepath.Join(root, "final")
	for _, dir := range []string{staged, final} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := types.WriteManifest(final, &types.CheckpointManifest{CheckpointID: "other"}); err != nil {
		t.Fatal(err)
	}

	if err := publishCheckpoint(staged, final, "checkpoint-123"); err == nil {
		t.Fatal("publishCheckpoint accepted conflicting artifact")
	}
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("staged artifact should remain for deferred cleanup: %v", err)
	}
}

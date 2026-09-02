// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrefetchCustomStorageArtifacts(t *testing.T) {
	checkpointDir := t.TempDir()
	processDir := filepath.Join(checkpointDir, "cuda-custom-storage", "process-nspid-42")
	if err := os.MkdirAll(processDir, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"device-0000.bin.part-0000": []byte("first"),
		"device-0000.bin.part-0001": []byte("second"),
		"manifest.txt":              []byte("ignored"),
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(processDir, name), contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	result, err := PrefetchCustomStorageArtifacts(context.Background(), checkpointDir)
	if err != nil {
		t.Fatal(err)
	}
	if result.Files != 2 || result.Bytes != int64(len("first")+len("second")) {
		t.Fatalf("unexpected prefetch result: %+v", result)
	}
}

func TestPrefetchCustomStorageArtifactsRejectsExtentSymlink(t *testing.T) {
	checkpointDir := t.TempDir()
	processDir := filepath.Join(checkpointDir, "cuda-custom-storage", "process-nspid-42")
	if err := os.MkdirAll(processDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(checkpointDir, "target")
	if err := os.WriteFile(target, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(processDir, "device-0000.bin")); err != nil {
		t.Fatal(err)
	}

	_, err := PrefetchCustomStorageArtifacts(context.Background(), checkpointDir)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
}

func TestPrefetchCustomStorageArtifactsHonorsCancellation(t *testing.T) {
	checkpointDir := t.TempDir()
	processDir := filepath.Join(checkpointDir, "cuda-custom-storage", "process-nspid-42")
	if err := os.MkdirAll(processDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(processDir, "device-0000.bin"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := PrefetchCustomStorageArtifacts(ctx, checkpointDir)
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func TestPrefetchCustomStorageArtifactsRejectsIncompleteArtifacts(t *testing.T) {
	tests := []struct {
		name      string
		prepare   func(t *testing.T, checkpointDir string)
		wantError string
	}{
		{
			name:      "missing artifact directory",
			prepare:   func(*testing.T, string) {},
			wantError: "inspect CUDA CustomStorage artifact directory",
		},
		{
			name: "no extent files",
			prepare: func(t *testing.T, checkpointDir string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(checkpointDir, "cuda-custom-storage"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
			wantError: "contains no extent files",
		},
		{
			name: "empty extent",
			prepare: func(t *testing.T, checkpointDir string) {
				t.Helper()
				processDir := filepath.Join(checkpointDir, "cuda-custom-storage", "process-nspid-42")
				if err := os.MkdirAll(processDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(processDir, "device-0000.bin"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantError: "not a nonempty regular file",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checkpointDir := t.TempDir()
			test.prepare(t, checkpointDir)
			_, err := PrefetchCustomStorageArtifacts(context.Background(), checkpointDir)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("expected error containing %q, got %v", test.wantError, err)
			}
		})
	}
}

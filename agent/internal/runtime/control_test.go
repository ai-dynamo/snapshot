// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteSentinelInDir_CreatesFileAtomically(t *testing.T) {
	dir := t.TempDir()

	if err := writeSentinelInDir(dir, "snapshot-complete", []byte("done\n")); err != nil {
		t.Fatalf("writeSentinelInDir failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "snapshot-complete"))
	if err != nil {
		t.Fatalf("sentinel not found: %v", err)
	}
	if string(data) != "done\n" {
		t.Errorf("unexpected sentinel contents: %q", data)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("failed to read dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "snapshot-complete" {
			t.Errorf("unexpected leftover file %q in control dir", e.Name())
		}
	}
}

func TestWriteSentinelInDir_Overwrites(t *testing.T) {
	dir := t.TempDir()
	if err := writeSentinelInDir(dir, "restore-complete", []byte("done\n")); err != nil {
		t.Fatalf("first write failed: %v", err)
	}
	if err := writeSentinelInDir(dir, "restore-complete", []byte("done\n")); err != nil {
		t.Fatalf("second write failed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "restore-complete"))
	if err != nil {
		t.Fatalf("sentinel not found: %v", err)
	}
	if string(data) != "done\n" {
		t.Errorf("unexpected sentinel contents: %q", data)
	}
}

func TestReadSentinelInDir_ReadsPayload(t *testing.T) {
	dir := t.TempDir()
	if err := writeSentinelInDir(dir, "restore-complete", []byte("43\n")); err != nil {
		t.Fatalf("writeSentinelInDir: %v", err)
	}
	data, err := readSentinelInDir(dir, "restore-complete")
	if err != nil {
		t.Fatalf("readSentinelInDir: %v", err)
	}
	if string(data) != "43\n" {
		t.Fatalf("readSentinelInDir() = %q, want 43\\n", data)
	}
}

func TestWriteSentinelInDir_DirMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if err := writeSentinelInDir(missing, "snapshot-complete", []byte("done\n")); err == nil {
		t.Fatal("expected error writing into missing directory")
	}
}

func TestWriteControlSentinel_RejectsInvalidPID(t *testing.T) {
	if err := WriteControlSentinel(0, "snapshot-complete"); err == nil {
		t.Fatal("expected error for PID 0")
	}
	if err := WriteControlSentinel(-1, "snapshot-complete"); err == nil {
		t.Fatal("expected error for negative PID")
	}
}

func TestControlSentinelExistsInDir(t *testing.T) {
	dir := t.TempDir()
	exists, err := controlSentinelExistsInDir(dir, "restore-complete")
	if err != nil {
		t.Fatalf("check missing sentinel: %v", err)
	}
	if exists {
		t.Fatal("missing sentinel reported as present")
	}

	if err := writeSentinelInDir(dir, "restore-complete", []byte("done\n")); err != nil {
		t.Fatalf("writeSentinelInDir: %v", err)
	}
	exists, err = controlSentinelExistsInDir(dir, "restore-complete")
	if err != nil {
		t.Fatalf("check existing sentinel: %v", err)
	}
	if !exists {
		t.Fatal("existing sentinel reported as missing")
	}
}

func TestControlSentinelExistsInDir_MissingDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := controlSentinelExistsInDir(missing, "restore-complete"); err == nil {
		t.Fatal("expected error for missing control mount")
	}
}

func TestRemoveControlSentinel_MissingFile(t *testing.T) {
	dir := t.TempDir()
	if err := RemoveControlSentinel(dir, "restore-complete"); err != nil {
		t.Fatalf("missing sentinel should be already removed: %v", err)
	}
}

func TestRemoveControlSentinel_RemovesExisting(t *testing.T) {
	dir := t.TempDir()
	if err := writeSentinelInDir(dir, "restore-complete", []byte("done\n")); err != nil {
		t.Fatalf("writeSentinelInDir: %v", err)
	}
	if err := RemoveControlSentinel(dir, "restore-complete"); err != nil {
		t.Fatalf("RemoveControlSentinel: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "restore-complete")); !os.IsNotExist(err) {
		t.Fatalf("sentinel should be gone, stat error: %v", err)
	}
}

func TestRemoveControlSentinel_MissingDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if err := RemoveControlSentinel(missing, "restore-complete"); err == nil {
		t.Fatal("expected error for missing control mount")
	}
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
)

func writeConfig(t *testing.T, path, document string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// The point of re-reading is that flipping the ConfigMap is enough: the kubelet
// updates the mounted file on its own, and the next restore sees the new value
// without the DaemonSet being rolled.
func TestNewSkipCompatCheckFnFollowsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, path, "restore:\n  skipCompatCheck: false\n")
	skip := NewSkipCompatCheckFn(path, false, logr.Discard())

	if skip() {
		t.Fatal("switch read true from a file that says false")
	}

	writeConfig(t, path, "restore:\n  skipCompatCheck: true\n")
	if !skip() {
		t.Fatal("switch did not follow the file being flipped on")
	}

	writeConfig(t, path, "restore:\n  skipCompatCheck: false\n")
	if skip() {
		t.Fatal("switch did not follow the file being flipped back off")
	}
}

// A restore is never failed, and never quietly checked differently, because a
// config read went wrong.
func TestNewSkipCompatCheckFnKeepsTheLastGoodValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	t.Run("missing file keeps what the agent started with", func(t *testing.T) {
		absent := filepath.Join(dir, "absent.yaml")
		if !NewSkipCompatCheckFn(absent, true, logr.Discard())() {
			t.Fatal("switch lost the startup value when the file was missing")
		}
		if NewSkipCompatCheckFn(absent, false, logr.Discard())() {
			t.Fatal("switch invented a value when the file was missing")
		}
	})

	t.Run("malformed file keeps the last value read", func(t *testing.T) {
		writeConfig(t, path, "restore:\n  skipCompatCheck: true\n")
		skip := NewSkipCompatCheckFn(path, false, logr.Discard())
		if !skip() {
			t.Fatal("switch read false from a file that says true")
		}

		writeConfig(t, path, "restore:\n\tskipCompatCheck: not-a-bool\n")
		if !skip() {
			t.Fatal("switch dropped the last known value on an unparseable file")
		}
	})
}

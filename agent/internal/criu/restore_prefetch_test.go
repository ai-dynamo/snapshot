// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPrefetchImageFiles(t *testing.T) {
	path := t.TempDir()
	want := bytes.Repeat([]byte("checkpoint"), 120000)
	for i := range 40 {
		if err := os.WriteFile(filepath.Join(path, fmt.Sprintf("ghost-file-%d.img", i)), want, 0400); err != nil {
			t.Fatal(err)
		}
	}
	// Non-images, page payloads and non-regular entries must not be read.
	for _, name := range []string{"pages-1.img", "cuda-checkpoint.bin"} {
		if err := os.WriteFile(filepath.Join(path, name), want, 0000); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("missing-target", filepath.Join(path, "link.img")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(path, "pipe.img"), 0600); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := prefetchImageFiles(dir); err != nil {
		t.Fatal(err)
	}
	for i := range 40 {
		got, err := os.ReadFile(filepath.Join(path, fmt.Sprintf("ghost-file-%d.img", i)))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("image %d changed: %v", i, err)
		}
	}
}

func TestPrefetchImageFileRejectsReplacedEntries(t *testing.T) {
	path := t.TempDir()
	if err := os.WriteFile(filepath.Join(path, "target"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(path, "link.img")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(path, "pipe.img"), 0600); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	for _, name := range []string{"link.img", "pipe.img", "missing.img"} {
		if err := prefetchImageFile(dir, name, make([]byte, 1024)); err == nil {
			t.Errorf("accepted %s after directory enumeration", name)
		}
	}
}

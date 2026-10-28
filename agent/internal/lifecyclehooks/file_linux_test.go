// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package lifecyclehooks

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// fakeContainer lays out procRoot/<pid>/root, as the agent sees a container
// through /host/proc, and returns procRoot, the PID and the root directory.
func fakeContainer(t *testing.T) (procRoot string, pid int, root string) {
	t.Helper()
	procRoot = t.TempDir()
	pid = 4242
	root = filepath.Join(procRoot, "4242", "root")
	if err := os.MkdirAll(filepath.Join(root, "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	return procRoot, pid, root
}

func TestFileExists(t *testing.T) {
	procRoot, pid, root := fakeContainer(t)
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "ready"))
	mustWrite(t, filepath.Join(root, "tmp", "ready"))
	mustMkdir(t, filepath.Join(root, "tmp", "dir"))
	mustSymlink(t, filepath.Join(root, "tmp", "ready"), filepath.Join(root, "tmp", "link"))
	mustSymlink(t, filepath.Join(root, "tmp"), filepath.Join(root, "linked-dir"))
	mustSymlink(t, outside, filepath.Join(root, "escape"))
	if err := unix.Mkfifo(filepath.Join(root, "tmp", "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		path   string
		exists bool
		fails  bool
	}{
		{"/tmp/ready", true, false},
		{"/tmp/missing", false, false},
		{"/missing-dir/ready", false, false},
		{"/tmp/dir", false, true},
		{"/tmp/fifo", false, true},
		{"/tmp/link", false, true},
		{"/linked-dir/ready", false, true},
		{"/escape/ready", false, true},
		{"/tmp/../tmp/ready", false, true},
		{"tmp/ready", false, true},
	} {
		t.Run(tc.path, func(t *testing.T) {
			exists, err := fileExists(procRoot, pid, tc.path)
			if tc.fails {
				if err == nil {
					t.Fatalf("fileExists(%q) = %v, nil, want an error", tc.path, exists)
				}
				return
			}
			if err != nil || exists != tc.exists {
				t.Fatalf("fileExists(%q) = %v, %v, want %v", tc.path, exists, err, tc.exists)
			}
		})
	}
}

func TestFileExistsWithoutTheContainer(t *testing.T) {
	procRoot, _, _ := fakeContainer(t)
	if _, err := fileExists(procRoot, 7, "/tmp/ready"); err == nil {
		t.Error("fileExists found a container that does not exist")
	}
	if _, err := fileExists(procRoot, 0, "/tmp/ready"); err == nil {
		t.Error("fileExists accepted PID 0")
	}
}

func TestCreateMarker(t *testing.T) {
	procRoot, pid, root := fakeContainer(t)
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)

	if err := createMarker(procRoot, pid, "/tmp/released"); err != nil {
		t.Fatalf("createMarker = %v, want nil", err)
	}
	info, err := os.Lstat(filepath.Join(root, "tmp", "released"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o644 || info.Size() != 0 {
		t.Fatalf("marker is %v with %d bytes, want an empty 0644 regular file", info.Mode(), info.Size())
	}
	if err := createMarker(procRoot, pid, "/released-at-root"); err != nil {
		t.Fatalf("createMarker at the root = %v, want nil", err)
	}
}

func TestCreateMarkerLeavesAnExistingFile(t *testing.T) {
	procRoot, pid, root := fakeContainer(t)
	marker := filepath.Join(root, "tmp", "released")
	if err := os.WriteFile(marker, []byte("app data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := createMarker(procRoot, pid, "/tmp/released"); !errors.Is(err, ErrExists) {
		t.Fatalf("createMarker = %v, want ErrExists", err)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "app data" {
		t.Fatalf("existing file changed to %q, %v", data, err)
	}
}

func TestCreateMarkerRefuses(t *testing.T) {
	procRoot, pid, root := fakeContainer(t)
	outside := t.TempDir()
	mustMkdir(t, filepath.Join(root, "tmp", "dir"))
	mustSymlink(t, filepath.Join(outside, "target"), filepath.Join(root, "tmp", "link"))
	mustSymlink(t, filepath.Join(root, "tmp"), filepath.Join(root, "linked-dir"))
	mustSymlink(t, outside, filepath.Join(root, "escape"))

	for _, tc := range []struct {
		name string
		path string
	}{
		{"missing parent", "/missing-dir/released"},
		{"directory at the path", "/tmp/dir"},
		{"symlink at the path", "/tmp/link"},
		{"symlink in the parent", "/linked-dir/released"},
		{"symlink out of the container", "/escape/released"},
		{"dot-dot", "/tmp/../released"},
		{"relative", "tmp/released"},
		{"root", "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := createMarker(procRoot, pid, tc.path)
			if err == nil || errors.Is(err, ErrExists) {
				t.Fatalf("createMarker(%q) = %v, want a refusal", tc.path, err)
			}
		})
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("createMarker wrote outside the container: %v, %v", entries, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "missing-dir")); !os.IsNotExist(err) {
		t.Fatalf("createMarker created a parent directory: %v", err)
	}
}

func mustWrite(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

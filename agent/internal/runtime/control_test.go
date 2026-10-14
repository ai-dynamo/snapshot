// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteSentinelInDir_CreatesFileAtomically(t *testing.T) {
	dir := t.TempDir()

	if err := writeSentinelInDir(dir, "snapshot-complete", []byte("pid=42\n")); err != nil {
		t.Fatalf("writeSentinelInDir failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "snapshot-complete"))
	if err != nil {
		t.Fatalf("sentinel not found: %v", err)
	}
	if string(data) != "pid=42\n" {
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
	if err := writeSentinelInDir(dir, "restore-complete", []byte("pid=1\n")); err != nil {
		t.Fatalf("first write failed: %v", err)
	}
	if err := writeSentinelInDir(dir, "restore-complete", []byte("pid=2\n")); err != nil {
		t.Fatalf("second write failed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "restore-complete"))
	if err != nil {
		t.Fatalf("sentinel not found: %v", err)
	}
	if string(data) != "pid=2\n" {
		t.Errorf("unexpected sentinel contents: %q", data)
	}
}

func TestWriteSentinelInDir_DirMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if err := writeSentinelInDir(missing, "snapshot-complete", nil); err == nil {
		t.Fatal("expected error writing into missing directory")
	}
}

func TestWriteControlSentinel_RejectsInvalidPID(t *testing.T) {
	if err := WriteControlSentinel(0, "snapshot-complete", nil); err == nil {
		t.Fatal("expected error for PID 0")
	}
	if err := WriteControlSentinel(-1, "snapshot-complete", nil); err == nil {
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

	if err := writeSentinelInDir(dir, "restore-complete", nil); err != nil {
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
	if err := writeSentinelInDir(dir, "restore-complete", nil); err != nil {
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

func TestPodControlVolumeDir(t *testing.T) {
	dir, err := podControlVolumeDir(KubeletPodsDir, "0f6c2c1e-uid", "engine-0")
	if err != nil {
		t.Fatalf("podControlVolumeDir: %v", err)
	}
	want := "/host/proc/1/root/var/lib/kubelet/pods/0f6c2c1e-uid/volumes/kubernetes.io~empty-dir/snapshot-control"
	if dir != want {
		t.Errorf("podControlVolumeDir = %q, want %q", dir, want)
	}

	for _, tc := range []struct{ uid, container string }{
		{"", "main"},
		{"uid", ""},
		{"..", "main"},
		{"uid", "../other"},
		{"a/b", "main"},
	} {
		if _, err := podControlVolumeDir(KubeletPodsDir, tc.uid, tc.container); err == nil {
			t.Errorf("podControlVolumeDir(%q, %q) succeeded, want error", tc.uid, tc.container)
		}
	}
}

// podsDirWithControlVolume returns a stand-in for KubeletPodsDir holding the
// snapshot-control volume of Pod "uid", as kubelet sets it up.
func podsDirWithControlVolume(t *testing.T) (podsDir, volume string) {
	t.Helper()
	podsDir = t.TempDir()
	volume, err := podControlVolumeDir(podsDir, "uid", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(volume, 0o755); err != nil {
		t.Fatal(err)
	}
	return podsDir, volume
}

func writeString(dir *os.File, name, contents string, perm os.FileMode) error {
	return WriteFileInDir(dir, name, perm, func(w io.Writer) error {
		_, err := io.WriteString(w, contents)
		return err
	})
}

func TestOpenPodControlDirCreatesContainerDir(t *testing.T) {
	podsDir, volume := podsDirWithControlVolume(t)

	dir, err := openPodControlDir(podsDir, "uid", "main")
	if err != nil {
		t.Fatalf("openPodControlDir: %v", err)
	}
	defer dir.Close()
	if err := writeString(dir, "restore-failed", "failed\n", 0o644); err != nil {
		t.Fatalf("WriteFileInDir: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(volume, "main", "restore-failed"))
	if err != nil || string(data) != "failed\n" {
		t.Fatalf("restore-failed = %q, %v", data, err)
	}
	entries, err := os.ReadDir(filepath.Join(volume, "main"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %v, %v; want only restore-failed", entries, err)
	}
}

func TestOpenPodControlDirRequiresVolume(t *testing.T) {
	if _, err := openPodControlDir(t.TempDir(), "uid", "main"); err == nil {
		t.Fatal("expected an error when kubelet has not set up the volume")
	}
}

// The Pod can change everything inside its control volume. A symlink there
// must not lead a write outside it.
func TestOpenPodControlDirRefusesSymlinkedContainerDir(t *testing.T) {
	podsDir, volume := podsDirWithControlVolume(t)
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(volume, "main")); err != nil {
		t.Fatal(err)
	}

	if dir, err := openPodControlDir(podsDir, "uid", "main"); err == nil {
		dir.Close()
		t.Fatal("expected an error for a symlinked container directory")
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("wrote outside the volume: %v", entries)
	}
}

func TestWriteFileInDirReplacesSymlinkWithoutFollowing(t *testing.T) {
	podsDir, volume := podsDirWithControlVolume(t)
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(volume, "main"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(volume, "main", "snapshot-restore-proxy")); err != nil {
		t.Fatal(err)
	}
	dir, err := openPodControlDir(podsDir, "uid", "main")
	if err != nil {
		t.Fatalf("openPodControlDir: %v", err)
	}
	defer dir.Close()

	if err := writeString(dir, "snapshot-restore-proxy", "proxy", 0o755); err != nil {
		t.Fatalf("WriteFileInDir: %v", err)
	}

	if data, _ := os.ReadFile(target); string(data) != "keep" {
		t.Fatalf("symlink target changed to %q", data)
	}
	info, err := os.Lstat(filepath.Join(volume, "main", "snapshot-restore-proxy"))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o755 {
		t.Fatalf("installed file = %v, %v; want a regular 0755 file", info, err)
	}
}

func TestWriteFileInDirRejectsPaths(t *testing.T) {
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	for _, name := range []string{"", "..", "a/b", "../x"} {
		if err := writeString(dir, name, "x", 0o644); err == nil {
			t.Errorf("WriteFileInDir(%q) succeeded, want error", name)
		}
	}
}

// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// fakeContainer lays out procRoot/<pid>/root/snapshot-control, as the agent
// sees a container through /host/proc, and returns procRoot, the PID and the
// control directory. The PID is arbitrary: nothing reads the real /proc.
func fakeContainer(t *testing.T) (procRoot string, pid int, controlDir string) {
	t.Helper()
	procRoot = t.TempDir()
	pid = 4242
	controlDir = filepath.Join(procRoot, "4242", "root", "snapshot-control")
	if err := os.MkdirAll(controlDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return procRoot, pid, controlDir
}

func readSentinel(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("sentinel not found: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("sentinel %s is %v, not a regular file", path, info.Mode())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestWriteControlSentinelCreatesAndOverwrites(t *testing.T) {
	procRoot, pid, controlDir := fakeContainer(t)

	for _, contents := range []string{"pid=1\n", "pid=2\n"} {
		if err := writeControlSentinel(procRoot, pid, "restore-complete", []byte(contents)); err != nil {
			t.Fatalf("writeControlSentinel: %v", err)
		}
		if got := readSentinel(t, filepath.Join(controlDir, "restore-complete")); got != contents {
			t.Errorf("sentinel = %q, want %q", got, contents)
		}
	}
	entries, err := os.ReadDir(controlDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("control dir has %d entries, want only the sentinel", len(entries))
	}
}

// Existing entries in the control directory are replaced, not written into.
func TestWriteControlSentinelReplacesExistingEntries(t *testing.T) {
	for name, entry := range map[string]string{
		"a temporary name": ".restore-complete.tmp",
		"the final name":   "restore-complete",
	} {
		t.Run(name, func(t *testing.T) {
			procRoot, pid, controlDir := fakeContainer(t)
			target := filepath.Join(t.TempDir(), "target")
			if err := os.WriteFile(target, []byte("original\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(controlDir, entry)); err != nil {
				t.Fatal(err)
			}

			if err := writeControlSentinel(procRoot, pid, "restore-complete", []byte("pid=42\n")); err != nil {
				t.Fatalf("writeControlSentinel: %v", err)
			}

			if got, _ := os.ReadFile(target); string(got) != "original\n" {
				t.Errorf("link target = %q, want it unchanged", got)
			}
			if got := readSentinel(t, filepath.Join(controlDir, "restore-complete")); got != "pid=42\n" {
				t.Errorf("sentinel = %q, want %q", got, "pid=42\n")
			}
		})
	}
}

// The control directory's path resolves inside the container's root.
func TestWriteControlSentinelResolvesInContainerRoot(t *testing.T) {
	procRoot, pid, controlDir := fakeContainer(t)
	root := filepath.Dir(controlDir)
	if err := os.Remove(controlDir); err != nil {
		t.Fatal(err)
	}
	inRoot := filepath.Join(root, "elsewhere")
	if err := os.Mkdir(inRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	// An absolute link resolves against the container's root.
	if err := os.Symlink("/elsewhere", controlDir); err != nil {
		t.Fatal(err)
	}

	if err := writeControlSentinel(procRoot, pid, "restore-failed", []byte("x\n")); err != nil {
		t.Fatalf("writeControlSentinel: %v", err)
	}

	if got := readSentinel(t, filepath.Join(inRoot, "restore-failed")); got != "x\n" {
		t.Errorf("sentinel = %q, want %q", got, "x\n")
	}
}

func TestWriteControlSentinelErrors(t *testing.T) {
	procRoot, pid, _ := fakeContainer(t)
	for name, tc := range map[string]struct {
		pid  int
		name string
	}{
		"PID 0":              {pid: 0, name: "restore-complete"},
		"negative PID":       {pid: -1, name: "restore-complete"},
		"no such container":  {pid: pid + 1, name: "restore-complete"},
		"a path, not a name": {pid: pid, name: "../restore-complete"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := writeControlSentinel(procRoot, tc.pid, tc.name, nil); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestWriteControlSentinelWithoutControlDir(t *testing.T) {
	procRoot, pid, controlDir := fakeContainer(t)
	if err := os.Remove(controlDir); err != nil {
		t.Fatal(err)
	}
	if err := writeControlSentinel(procRoot, pid, "restore-complete", nil); err == nil {
		t.Fatal("expected an error for a missing control mount")
	}
}

func TestControlSentinelExists(t *testing.T) {
	procRoot, pid, controlDir := fakeContainer(t)
	check := func(wantExists, wantErr bool) {
		t.Helper()
		exists, err := controlSentinelExists(procRoot, pid, "restore-complete")
		if (err != nil) != wantErr {
			t.Fatalf("err = %v, want error %v", err, wantErr)
		}
		if exists != wantExists {
			t.Fatalf("exists = %v, want %v", exists, wantExists)
		}
	}

	check(false, false) // nothing there

	path := filepath.Join(controlDir, "restore-complete")
	if err := os.Symlink(filepath.Join(t.TempDir(), "anything"), path); err != nil {
		t.Fatal(err)
	}
	check(false, true) // not a regular file

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	check(true, false)
}

func TestControlSentinelExistsWithoutControlDir(t *testing.T) {
	procRoot, pid, controlDir := fakeContainer(t)
	if err := os.Remove(controlDir); err != nil {
		t.Fatal(err)
	}
	if _, err := controlSentinelExists(procRoot, pid, "restore-complete"); err == nil {
		t.Fatal("expected an error for a missing control mount")
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
	if err := os.WriteFile(filepath.Join(dir, "restore-complete"), nil, 0o644); err != nil {
		t.Fatal(err)
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

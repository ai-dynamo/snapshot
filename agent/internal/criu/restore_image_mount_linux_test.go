// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package criu

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/go-logr/logr"
	"golang.org/x/sys/unix"
)

func TestMountFilesImageWithOps(t *testing.T) {
	const mountFD = 42
	target := filepath.Join("/checkpoint", filesImageFilename)
	var calls []string
	ops := mountFilesImageOps{
		openTree: func(dirfd int, path string, flags uint) (int, error) {
			if dirfd != unix.AT_FDCWD || path != "/rewrite/files.img" || flags != unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC {
				t.Fatalf("unexpected open_tree arguments: %d %q %#x", dirfd, path, flags)
			}
			calls = append(calls, "open")
			return mountFD, nil
		},
		mountSetattr: func(fd int, path string, flags uint, attr *unix.MountAttr) error {
			want := uint64(unix.MOUNT_ATTR_RDONLY | unix.MOUNT_ATTR_NOSUID | unix.MOUNT_ATTR_NODEV | unix.MOUNT_ATTR_NOEXEC)
			if fd != mountFD || path != "" || flags != unix.AT_EMPTY_PATH || attr.Attr_set != want {
				t.Fatalf("unexpected mount_setattr arguments: %d %q %#x %#x", fd, path, flags, attr.Attr_set)
			}
			calls = append(calls, "attrs")
			return nil
		},
		moveMount: func(fromFD int, fromPath string, toFD int, toPath string, flags int) error {
			if fromFD != mountFD || fromPath != "" || toFD != unix.AT_FDCWD || toPath != target || flags != unix.MOVE_MOUNT_F_EMPTY_PATH {
				t.Fatalf("unexpected move_mount arguments: %d %q %d %q %#x", fromFD, fromPath, toFD, toPath, flags)
			}
			calls = append(calls, "move")
			return nil
		},
		unmount: func(path string, flags int) error {
			if path != target || flags != unix.MNT_DETACH {
				t.Fatalf("unexpected unmount arguments: %q %#x", path, flags)
			}
			calls = append(calls, "unmount")
			return nil
		},
		closeFD: func(fd int) error {
			if fd != mountFD {
				t.Fatalf("close fd = %d, want %d", fd, mountFD)
			}
			calls = append(calls, "close")
			return nil
		},
	}

	cleanup, err := mountFilesImageWithOps("/checkpoint", "/rewrite/files.img", ops)
	if err != nil {
		t.Fatal(err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	want := []string{"open", "attrs", "move", "close", "unmount"}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("calls = %v, want %v", calls, want)
		}
	}
}

func TestMountFilesImageClosesDetachedMountOnFailure(t *testing.T) {
	wantErr := errors.New("mount failed")
	for _, failMove := range []bool{false, true} {
		t.Run(map[bool]string{false: "attributes", true: "move"}[failMove], func(t *testing.T) {
			closed := 0
			ops := mountFilesImageOps{
				openTree: func(int, string, uint) (int, error) { return 42, nil },
				mountSetattr: func(int, string, uint, *unix.MountAttr) error {
					if !failMove {
						return wantErr
					}
					return nil
				},
				moveMount: func(int, string, int, string, int) error { return wantErr },
				closeFD:   func(int) error { closed++; return nil },
			}
			cleanup, err := mountFilesImageWithOps("/checkpoint", "/rewrite/files.img", ops)
			if !errors.Is(err, wantErr) || cleanup != nil || closed != 1 {
				t.Fatalf("has cleanup=%t error=%v closed=%d", cleanup != nil, err, closed)
			}
		})
	}
}

func TestMountFilesImageCleanupErrors(t *testing.T) {
	for _, tc := range []struct {
		err     error
		wantErr bool
	}{{unix.ENOENT, false}, {unix.EINVAL, false}, {unix.EPERM, true}} {
		ops := mountFilesImageOps{
			openTree:     func(int, string, uint) (int, error) { return 42, nil },
			mountSetattr: func(int, string, uint, *unix.MountAttr) error { return nil },
			moveMount:    func(int, string, int, string, int) error { return nil },
			unmount:      func(string, int) error { return tc.err },
			closeFD:      func(int) error { return nil },
		}
		cleanup, err := mountFilesImageWithOps("/checkpoint", "/rewrite/files.img", ops)
		if err != nil {
			t.Fatal(err)
		}
		if err := cleanup(); (err != nil) != tc.wantErr {
			t.Fatalf("cleanup error = %v, wantErr %t", err, tc.wantErr)
		}
	}
}

func TestMountFilesImageAcrossDevices(t *testing.T) {
	if os.Getenv("SNAPSHOT_PRIVILEGED_TESTS") != "1" {
		t.Skip("set SNAPSHOT_PRIVILEGED_TESTS=1 in a privileged Linux environment")
	}

	root := t.TempDir()
	checkpoint := filepath.Join(root, "checkpoint")
	if err := os.Mkdir(checkpoint, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mount("tmpfs", checkpoint, "tmpfs", unix.MS_NODEV|unix.MS_NOSUID, "size=1m"); err != nil {
		t.Fatal(err)
	}
	defer unix.Unmount(checkpoint, unix.MNT_DETACH) //nolint:errcheck

	original := filepath.Join(checkpoint, filesImageFilename)
	pages := filepath.Join(checkpoint, "pages-1.img")
	replacement := filepath.Join(root, "replacement.img")
	for path, data := range map[string]string{original: "original", pages: "pages", replacement: "rewritten"} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pageStat, replacementStat := statFile(t, pages), statFile(t, replacement)
	if pageStat.Dev == replacementStat.Dev {
		t.Fatalf("test requires different devices, both are %d", pageStat.Dev)
	}
	if err := unix.MountSetattr(unix.AT_FDCWD, checkpoint, 0, &unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY}); err != nil {
		t.Fatal(err)
	}

	cleanup, err := mountFilesImage(checkpoint, replacement)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup() //nolint:errcheck
	if err := os.Remove(replacement); err != nil {
		t.Fatal(err)
	}
	viewedFiles, viewedPages := statFile(t, original), statFile(t, pages)
	if viewedFiles.Dev != replacementStat.Dev || viewedFiles.Ino != replacementStat.Ino {
		t.Fatalf("files.img inode = %d:%d, want %d:%d", viewedFiles.Dev, viewedFiles.Ino, replacementStat.Dev, replacementStat.Ino)
	}
	if viewedPages.Dev != pageStat.Dev || viewedPages.Ino != pageStat.Ino {
		t.Fatalf("pages inode changed from %d:%d to %d:%d", pageStat.Dev, pageStat.Ino, viewedPages.Dev, viewedPages.Ino)
	}
	if _, err := os.OpenFile(original, os.O_WRONLY, 0); !errors.Is(err, unix.EROFS) {
		t.Fatalf("write error = %v, want EROFS", err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(original)
	if err != nil || string(data) != "original" {
		t.Fatalf("original files.img after cleanup = %q, %v", data, err)
	}
}

func TestGPUDeviceMountsAcrossNamespaces(t *testing.T) {
	if os.Getenv("SNAPSHOT_PRIVILEGED_TESTS") != "1" {
		t.Skip("set SNAPSHOT_PRIVILEGED_TESTS=1 in a privileged Linux environment")
	}
	cases := map[string]map[string]string{
		"single":  {"/dev/nvidia0": "/dev/nvidia1"},
		"overlap": {"/dev/nvidia0": "/dev/nvidia1", "/dev/nvidia1": "/dev/nvidia2"},
		"swap":    {"/dev/nvidia0": "/dev/nvidia1", "/dev/nvidia1": "/dev/nvidia0"},
	}
	if name := os.Getenv("SNAPSHOT_GPU_MOUNT_TEST"); name != "" {
		testGPUDeviceMounts(t, cases[name])
		return
	}
	for name := range cases {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestGPUDeviceMountsAcrossNamespaces$", "-test.v")
			cmd.Env = append(os.Environ(), "SNAPSHOT_GPU_MOUNT_TEST="+name)
			cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWNS}
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("isolated mount test: %v\n%s", err, output)
			}
		})
	}
}

func testGPUDeviceMounts(t *testing.T, aliases map[string]string) {
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// The subprocess owns this namespace; no /dev changes reach the test runner.
	must(unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""))
	must(unix.Mount("tmpfs", "/dev", "tmpfs", 0, "size=1m"))
	backing := "/dev/backing"
	must(os.Mkdir(backing, 0o700))
	must(unix.Mknod("/dev/null", unix.S_IFCHR|0o666, int(unix.Mkdev(1, 3))))
	want := map[string]uint64{}
	for i, minor := range []uint32{3, 5, 7} {
		path := fmt.Sprintf("/dev/nvidia%d", i)
		if len(aliases) == 1 && i == 0 {
			continue // checkpoint path is absent in a single-GPU destination
		}
		device := filepath.Join(backing, filepath.Base(path))
		rdev := unix.Mkdev(1, minor) // harmless character devices, not real GPUs
		must(unix.Mknod(device, unix.S_IFCHR|0o600, int(rdev)))
		must(os.WriteFile(path, nil, 0o600))
		must(unix.Mount(device, path, "", unix.MS_BIND, ""))
		want[path] = rdev
	}
	m, err := PrepareGPUDeviceMounts(aliases, logr.Discard())
	must(err)
	var pinnedFDs []uintptr
	for _, f := range m.devices {
		pinnedFDs = append(pinnedFDs, f.Fd())
	}
	for source, destination := range aliases {
		if st := statFile(t, source); st.Rdev != want[destination] {
			t.Fatalf("alias %s points at %d, want %d", source, st.Rdev, want[destination])
		}
	}
	must(m.Close(false))
	for _, fd := range pinnedFDs {
		if _, err := unix.FcntlInt(fd, unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
			t.Fatalf("pin was not closed: %v", err)
		}
	}
	for source := range aliases {
		var st unix.Stat_t
		err := unix.Stat(source, &st)
		if _, existed := want[source]; !existed {
			if !errors.Is(err, unix.ENOENT) {
				t.Fatalf("created alias was not removed: %v", err)
			}
		} else if err != nil || st.Rdev != want[source] {
			t.Fatalf("rollback changed %s: rdev=%d err=%v", source, st.Rdev, err)
		}
	}
	m, err = PrepareGPUDeviceMounts(aliases, logr.Discard())
	must(err)
	defer func() { must(m.Close(true)) }()

	// Preserve checkpoint aliases, then replay /dev non-recursively as CRIU
	// does. Native mounts disappear while checkpoint paths are mounted again.
	replayed := map[string]*os.File{}
	for path := range aliases {
		f, err := os.OpenFile(path, unix.O_PATH|unix.O_CLOEXEC, 0)
		must(err)
		defer f.Close()
		replayed[path] = f
	}
	must(unix.Mount("/dev", "/dev", "", unix.MS_BIND, ""))
	for path, f := range replayed {
		must(unix.Mount(fmt.Sprintf("/proc/self/fd/%d", f.Fd()), path, "", unix.MS_BIND, ""))
	}
	before := map[string]unix.Stat_t{}
	for _, path := range aliases {
		before[path] = statFile(t, path)
	}
	// Two namespaces, each with two processes sharing its /dev.
	for range 2 {
		target := exec.Command("sh", "-c", "sleep 60 &\necho $!\nwait")
		target.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWNS, Setpgid: true}
		stdout, err := target.StdoutPipe()
		must(err)
		must(target.Start())
		defer func() {
			_ = unix.Kill(-target.Process.Pid, unix.SIGKILL)
			_ = target.Wait()
		}()
		var childPID int
		_, err = fmt.Fscan(stdout, &childPID)
		must(err)
		var mounts []byte
		for _, pid := range []int{target.Process.Pid, childPID} {
			must(m.RestoreNativePaths(pid))
			current, err := os.ReadFile(fmt.Sprintf("/proc/%d/mountinfo", pid))
			must(err)
			if mounts != nil && !bytes.Equal(mounts, current) {
				t.Fatal("mounted devices again for a process sharing the mount namespace")
			}
			mounts = current
			for _, path := range aliases {
				st := statFile(t, fmt.Sprintf("/proc/%d/root%s", pid, path))
				if st.Mode&unix.S_IFMT != unix.S_IFCHR || st.Rdev != want[path] {
					t.Fatalf("%s: mode=%o rdev=%d, want character device %d", path, st.Mode, st.Rdev, want[path])
				}
				if st = statFile(t, path); st.Rdev != before[path].Rdev || st.Mode != before[path].Mode {
					t.Fatalf("modified caller's device path %s", path)
				}
			}
		}
	}
}

func statFile(t *testing.T, path string) unix.Stat_t {
	t.Helper()
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		t.Fatal(err)
	}
	return stat
}

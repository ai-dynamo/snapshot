// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package criu

import (
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

func TestGPUDeviceMountsWithoutAliases(t *testing.T) {
	m, err := PrepareGPUDeviceMounts(nil, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RestoreNativePaths(-1); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(true); err != nil {
		t.Fatal(err)
	}
}

func TestGPUDeviceMountsRejectInvalidPaths(t *testing.T) {
	for _, aliases := range []map[string]string{
		{"/dev/nvidia0": "/dev/null"},
		{"/tmp/nvidia0": "/dev/nvidia1"},
	} {
		if m, err := PrepareGPUDeviceMounts(aliases, logr.Discard()); err == nil {
			_ = m.Close(false)
			t.Fatal("accepted an invalid GPU path")
		}
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
	t.Helper()
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
		var st unix.Stat_t
		must(unix.Stat(source, &st))
		if st.Rdev != want[destination] {
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
	// Use a separate target namespace, not the caller's /dev.
	target := exec.Command("sleep", "60")
	target.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWNS}
	must(target.Start())
	defer func() {
		_ = target.Process.Kill()
		_ = target.Wait()
	}()
	before := map[string]unix.Stat_t{}
	for _, path := range aliases {
		var st unix.Stat_t
		must(unix.Stat(path, &st))
		before[path] = st
	}
	for range 2 { // repeated CUDA processes in one namespace must also work
		must(m.RestoreNativePaths(target.Process.Pid))
		for _, path := range aliases {
			var st unix.Stat_t
			must(unix.Stat(fmt.Sprintf("/proc/%d/root%s", target.Process.Pid, path), &st))
			if st.Mode&unix.S_IFMT != unix.S_IFCHR || st.Rdev != want[path] {
				t.Fatalf("%s: mode=%o rdev=%d, want character device %d", path, st.Mode, st.Rdev, want[path])
			}
			must(unix.Stat(path, &st))
			if st.Rdev != before[path].Rdev || st.Mode != before[path].Mode {
				t.Fatalf("modified caller's device path %s", path)
			}
		}
	}
}

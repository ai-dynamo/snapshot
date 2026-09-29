// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Exercise the kernel contract used by execNSRestore without requiring setns
// privileges or CRIU: ExtraFiles survive entry, parent fd paths survive pathname
// replacement, and CLOEXEC keeps the pinned directories out of helpers.
func TestRestorePinnedCUDADirectories(t *testing.T) {
	if os.Getenv("SNAPSHOT_TEST_RESTORE_FDS") == "1" {
		inherited := exec.Command("/bin/sh", "-c", "test -d /proc/self/fd/4 && test -d /proc/self/fd/5 && test -d /proc/self/fd/6")
		if output, err := inherited.CombinedOutput(); err != nil {
			t.Fatalf("ExtraFiles descriptors did not survive exec: %v: %s", err, output)
		}
		syscall.CloseOnExec(4)
		syscall.CloseOnExec(5)
		syscall.CloseOnExec(6)
		bundle := fmt.Sprintf("/proc/%d/fd/4", os.Getpid())
		driver := fmt.Sprintf("/proc/%d/fd/5", os.Getpid())
		companions := fmt.Sprintf("/proc/%d/fd/6", os.Getpid())
		if err := os.Symlink(driver, filepath.Join(companions, "host")); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("/bin/sh", "-c", `
test ! -e /proc/self/fd/4 && test ! -e /proc/self/fd/5 && test ! -e /proc/self/fd/6 &&
test "$(cat "$1/helper")" = trusted-helper &&
test "$(cat "$2/libcuda.so.1")" = trusted-driver &&
test "$(cat "$3/libnvidia-ptxjitcompiler.so.1")" = trusted-companion &&
test ! -e "$3/libc.so.6"
`, "restore-fds", bundle, driver, companions)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("helper could not use pinned directories: %v: %s", err, output)
		}
		return
	}

	root := t.TempDir()
	bundlePath := filepath.Join(root, "bundle")
	driverPath := filepath.Join(root, "driver")
	companionsPath := filepath.Join(root, "companions")
	for _, path := range []string{bundlePath, driverPath, companionsPath} {
		if err := os.Mkdir(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	for path, contents := range map[string]string{
		filepath.Join(bundlePath, "helper"):                        "trusted-helper",
		filepath.Join(driverPath, "libcuda.so.1"):                  "trusted-driver",
		filepath.Join(driverPath, "libnvidia-ptxjitcompiler.so.1"): "trusted-companion",
		filepath.Join(driverPath, "libc.so.6"):                     "agent-libc",
	} {
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := linkHostCUDACompanions(driverPath, companionsPath); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, filepath.Join(bundlePath, "nsrestore")); err != nil {
		t.Fatal(err)
	}
	var files []*os.File
	for _, path := range []string{os.DevNull, bundlePath, driverPath, companionsPath} {
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = file.Close() })
		files = append(files, file)
	}
	for _, path := range []string{bundlePath, driverPath, companionsPath} {
		if err := os.Rename(path, path+"-original"); err != nil {
			t.Fatal(err)
		}
		// Replacing the original pathname must not redirect the pinned tree.
		if err := os.Mkdir(path, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "helper"), []byte("untrusted"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("/proc/self/fd/4/nsrestore", "-test.run=^TestRestorePinnedCUDADirectories$")
	cmd.ExtraFiles = files
	cmd.Env = append(os.Environ(), "SNAPSHOT_TEST_RESTORE_FDS=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("execute through inherited bundle: %v: %s", err, output)
	}
}

func TestLinkHostCUDACompanions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
		want   string
	}{
		{name: "relative alias", target: "libnvidia-test.so.999"},
		{name: "absolute internal alias", target: "absolute"},
		{name: "escaping alias", target: "../libnvidia-outside.so.1", want: "not a regular file in the host driver directory"},
		{name: "broken alias", target: "missing", want: "resolve CUDA companion"},
		{name: "directory alias", target: ".", want: "not a regular file in the host driver directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			host, view := filepath.Join(root, "host"), filepath.Join(root, "view")
			for _, dir := range []string{host, view} {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			for _, path := range []string{filepath.Join(host, "libnvidia-test.so.999"), filepath.Join(host, "libc.so.6"), filepath.Join(root, "libnvidia-outside.so.1")} {
				if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			target := tc.target
			if target == "absolute" {
				target = filepath.Join(host, "libnvidia-test.so.999")
			}
			if err := os.Symlink(target, filepath.Join(host, "libnvidia-test.so.1")); err != nil {
				t.Fatal(err)
			}
			err := linkHostCUDACompanions(host, view)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("link error = %v, want %q", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"libnvidia-test.so.1", "libnvidia-test.so.999"} {
				target, err := os.Readlink(filepath.Join(view, name))
				if err != nil || target != "host/libnvidia-test.so.999" {
					t.Fatalf("alias %s = %q, %v", name, target, err)
				}
			}
			if _, err := os.Lstat(filepath.Join(view, "libc.so.6")); !os.IsNotExist(err) {
				t.Fatalf("agent libc exposed in NVIDIA search path: %v", err)
			}
		})
	}
}

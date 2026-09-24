// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package cuda

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestResolveDevicePathsValidatesPhysicalMinor(t *testing.T) {
	root := t.TempDir()
	infoDir := filepath.Join(root, "driver/nvidia/gpus/0000:41:00.0")
	deviceDir := filepath.Join(root, "100/root/dev")
	for _, dir := range []string{infoDir, deviceDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(infoDir, "information"),
		[]byte("GPU UUID: GPU-A\nDevice Minor: 7\n"), 0644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(deviceDir, "nvidia7")
	if err := unix.Mknod(path, unix.S_IFCHR|0600, int(unix.Mkdev(195, 7))); err != nil {
		if errors.Is(err, unix.EPERM) {
			t.Skip("requires permission to create a test character device")
		}
		t.Fatal(err)
	}
	got, err := ResolveDevicePaths(root, 100, []string{"GPU-A"})
	if err != nil || got["GPU-A"] != "/dev/nvidia7" {
		t.Fatalf("paths = %v, error = %v", got, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveDevicePaths(root, 100, []string{"GPU-A"}); err == nil {
		t.Fatal("accepted a regular file in place of the allocated GPU")
	}
}

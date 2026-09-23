// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package criu

import (
	"errors"
	"os"
	"testing"

	"github.com/checkpoint-restore/go-criu/v8/crit/images/fdinfo"
	"github.com/go-logr/logr"
	"golang.org/x/sys/unix"
)

func TestRestoreImageMountUnsupported(t *testing.T) {
	checkpoint, scratch := t.TempDir(), t.TempDir()
	writeFilesImage(t, checkpoint, []*fdinfo.FileEntry{
		newUnixSocketEntry(1, []byte("\x00restore"), 101, unix.SOCK_STREAM, linuxUnixSocketStateListen),
	})
	_, cleanup, err := prepareRestoreImageDirForRestoreID(checkpoint, 1, scratch)
	if !errors.Is(err, errors.ErrUnsupported) || cleanup != nil {
		t.Fatalf("image mount error = %v, cleanup present %v; want unsupported, no cleanup", err, cleanup != nil)
	}
	files, err := os.ReadDir(scratch)
	if err != nil || len(files) != 0 {
		t.Fatalf("failed mount left temporary images: %v, %v", files, err)
	}
}

func TestGPUDeviceMountsUnsupported(t *testing.T) {
	mounts, err := PrepareGPUDeviceMounts(map[string]string{"/dev/nvidia0": "/dev/nvidia1"}, logr.Discard())
	if !errors.Is(err, errors.ErrUnsupported) || mounts != nil {
		t.Fatalf("PrepareGPUDeviceMounts = %v, %v; want nil, unsupported", mounts, err)
	}
	file, err := os.CreateTemp(t.TempDir(), "device")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	mounts = &GPUDeviceMounts{devices: map[string]*os.File{"/dev/nvidia1": file}}
	if err := mounts.RestoreNativePaths(1); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("RestoreNativePaths = %v; want unsupported", err)
	}
	if err := mounts.Close(false); err != nil {
		t.Fatalf("Close must still release pinned files: %v", err)
	}
	if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("pinned file was not closed: %v", err)
	}
}

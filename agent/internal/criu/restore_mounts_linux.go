// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package criu

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"golang.org/x/sys/unix"
)

// RestoreNativePaths puts allocated devices at their native paths before CUDA
// restore. CRIU replays checkpoint mounts, which can hide destination-only paths
// under a new /dev. Native paths also take precedence over temporary aliases in
// overlapping mappings once CRIU has consumed those aliases.
func (m *GPUDeviceMounts) RestoreNativePaths(pid int) error {
	if len(m.devices) == 0 {
		return nil
	}
	ns, err := os.Open(fmt.Sprintf("/proc/%d/ns/mnt", pid))
	if err != nil {
		return err
	}
	defer ns.Close()
	var nsStat unix.Stat_t
	if err := unix.Fstat(int(ns.Fd()), &nsStat); err != nil {
		return err
	}
	// CUDA processes can share a mount namespace. Install each set of mounts once.
	if m.restoredNS[nsStat.Ino] {
		return nil
	}
	root, err := os.Open(fmt.Sprintf("/proc/%d/root", pid))
	if err != nil {
		return err
	}
	defer root.Close()
	devFD, err := unix.Openat(int(root.Fd()), "dev", unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open restored /dev for pid %d: %w", pid, err)
	}
	defer unix.Close(devFD)

	// Detached mounts can cross namespaces; ordinary bind mounts from the
	// placeholder namespace cannot. Clone from the pins, never the aliased paths.
	mounts := make(map[string]int)
	defer func() {
		for _, fd := range mounts {
			_ = unix.Close(fd)
		}
	}()
	for path, f := range m.devices {
		fd, err := unix.OpenTree(int(f.Fd()), "", unix.AT_EMPTY_PATH|unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC)
		if err != nil {
			return fmt.Errorf("clone GPU mount %s: %w", path, err)
		}
		mounts[path] = fd
	}
	done := make(chan error, 1)
	go func() {
		// Mount-namespace setns requires private filesystem state. Do not unlock:
		// Go terminates this thread on return rather than reusing its namespace.
		runtime.LockOSThread()
		if err := unix.Unshare(unix.CLONE_FS); err != nil {
			done <- err
			return
		}
		if err := unix.Setns(int(ns.Fd()), unix.CLONE_NEWNS); err != nil {
			done <- err
			return
		}
		for path, fd := range mounts {
			if err := installGPUDeviceMount(devFD, filepath.Base(path), fd); err != nil {
				done <- fmt.Errorf("restore native GPU path %s for pid %d: %w", path, pid, err)
				return
			}
		}
		done <- nil
	}()
	if err := <-done; err != nil {
		return err
	}
	if m.restoredNS == nil {
		m.restoredNS = make(map[uint64]bool)
	}
	m.restoredNS[nsStat.Ino] = true
	return nil
}

func installGPUDeviceMount(devFD int, name string, mountFD int) error {
	if err := unix.Mknodat(devFD, name, unix.S_IFREG|0o600, 0); err != nil && !errors.Is(err, unix.EEXIST) {
		return err
	}
	targetFD, err := unix.Openat(devFD, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(targetFD)
	var st unix.Stat_t
	if err := unix.Fstat(targetFD, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG && st.Mode&unix.S_IFMT != unix.S_IFCHR {
		return fmt.Errorf("GPU mountpoint is not a regular file or character device")
	}
	return unix.MoveMount(mountFD, "", targetFD, "", unix.MOVE_MOUNT_F_EMPTY_PATH|unix.MOVE_MOUNT_T_EMPTY_PATH)
}

func openGPUDevice(path string) (*os.File, error) {
	return os.OpenFile(path, unix.O_PATH|unix.O_CLOEXEC, 0)
}

func bindGPUDevice(source, target string) error {
	return unix.Mount(source, target, "", unix.MS_BIND, "")
}

func detachGPUDevice(path string) error {
	return unix.Unmount(path, unix.MNT_DETACH)
}

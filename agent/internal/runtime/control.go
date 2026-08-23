// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// WriteControlSentinel writes a sentinel file into the workload container's
// snapshot-control volume at SnapshotControlMountPath/<name>, accessed through
// the agent's /host/proc/<pid>/root view of the container's mount namespace.
//
// hostPID must be a PID inside the container's mount namespace (the container
// task PID is the canonical choice). The sentinel is observed by the workload
// via inotify on the control directory; it replaces the SIGUSR1/SIGCONT
// agent-to-workload signals that previously required the workload to run as
// PID 1.
//
// The write uses create-then-rename so the workload never observes a partial
// file.
func WriteControlSentinel(hostPID int, name string) error {
	if hostPID <= 0 {
		return fmt.Errorf("invalid host PID %d for control sentinel %q", hostPID, name)
	}
	dir := filepath.Join(HostProcPath, strconv.Itoa(hostPID), "root", snapshotv1alpha1.SnapshotControlMountPath)
	return writeSentinelInDir(dir, name)
}

// ControlSentinelExists reports whether a sentinel exists in the workload
// container's snapshot-control volume. It returns an error when the container's
// control mount cannot be inspected, so callers do not mistake an inaccessible
// volume for a missing sentinel.
func ControlSentinelExists(hostPID int, name string) (bool, error) {
	if hostPID <= 0 {
		return false, fmt.Errorf("invalid host PID %d for control sentinel %q", hostPID, name)
	}
	dir := filepath.Join(HostProcPath, strconv.Itoa(hostPID), "root", snapshotv1alpha1.SnapshotControlMountPath)
	return controlSentinelExistsInDir(dir, name)
}

// RemoveControlSentinel removes a sentinel from the snapshot-control mount at
// dir. The directory must exist: a missing file is already removed, but a
// missing mount is an error so callers do not confuse "cannot see the volume"
// with "volume is clean".
func RemoveControlSentinel(dir, name string) error {
	return removeSentinelInDir(dir, name)
}

func writeSentinelInDir(dir, name string) error {
	tmpPath := filepath.Join(dir, "."+name+".tmp")
	finalPath := filepath.Join(dir, name)
	if err := os.WriteFile(tmpPath, []byte("done\n"), 0o644); err != nil {
		return fmt.Errorf("write temp sentinel %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename sentinel %s -> %s: %w", tmpPath, finalPath, err)
	}
	return nil
}

func removeSentinelInDir(dir, name string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("control sentinel dir %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("control sentinel dir %s: not a directory", dir)
	}
	path := filepath.Join(dir, name)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove control sentinel %s: %w", path, err)
	}
	return nil
}

func controlSentinelExistsInDir(dir, name string) (bool, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return false, fmt.Errorf("control sentinel dir %s: %w", dir, err)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("control sentinel dir %s: not a directory", dir)
	}
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat control sentinel %s: %w", path, err)
	}
	return true, nil
}

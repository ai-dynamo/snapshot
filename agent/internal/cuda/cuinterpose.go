// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

const (
	CoordinatorBinaryName        = "cuinterpose-coordinator"
	DefaultCoordinatorBinaryPath = "/usr/local/bin/" + CoordinatorBinaryName
)

// RemoveStaleCuinterposeSockets clears the exact endpoints CRIU's restored PIDs
// will bind, leaving other processes' sockets and control files untouched.
func RemoveStaleCuinterposeSockets(controlDir string, namespacePIDs []int) error {
	for _, pid := range namespacePIDs {
		path := filepath.Join(controlDir, fmt.Sprintf("cuinterpose-%d.sock", pid))
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale cuinterpose socket: %w", err)
		}
	}
	return nil
}

// PrepareCuinterpose runs before native CUDA checkpoint. Open the executable,
// artifact directory, mount namespace, and container root before namespace entry.
// A failed prepare cannot be rolled back; the caller terminates the source.
func PrepareCuinterpose(ctx context.Context, checkpointDir, procRoot string, targetPID int, namespacePIDs []int, binary string) error {
	processDir := filepath.Join(procRoot, strconv.Itoa(targetPID))
	var files []*os.File
	defer func() {
		for _, file := range files {
			_ = file.Close()
		}
	}()
	// ExtraFiles become child descriptors 3 through 6, in this order.
	for _, path := range []string{binary, checkpointDir, filepath.Join(processDir, "ns/mnt"), filepath.Join(processDir, "root")} {
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open cuinterpose prepare input: %w", err)
		}
		files = append(files, file)
	}
	args := []string{
		"--mount=/proc/self/fd/5", "-t", strconv.Itoa(targetPID), "-u", "-i", "-n", "-p",
		// Entering a mount namespace alone does not change the filesystem root.
		"--root=/proc/self/fd/6", "--wd=/proc/self/fd/6",
		"--", "/proc/self/fd/3",
	}
	args = append(args, cuinterposeArgs("prepare", "/proc/self/fd/4", namespacePIDs)...)
	cmd := exec.CommandContext(ctx, "nsenter", args...)
	cmd.ExtraFiles = files
	return executeCoordinator(cmd)
}

// RestoreCuinterpose runs after native CUDA restore/unlock, inside the restored
// namespaces. binary is a descriptor path opened before CRIU replaced mounts.
func RestoreCuinterpose(ctx context.Context, checkpointDir string, namespacePIDs []int, binary string) error {
	return executeCoordinator(exec.CommandContext(ctx, binary, cuinterposeArgs("restore", checkpointDir, namespacePIDs)...))
}

func executeCoordinator(cmd *exec.Cmd) error {
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cuinterpose coordinator: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func cuinterposeArgs(operation, checkpointDir string, namespacePIDs []int) []string {
	args := []string{"--" + operation, "--checkpoint-dir", checkpointDir, "--control-dir", podcontract.SnapshotControlMountPath}
	for _, pid := range namespacePIDs {
		args = append(args, "--process", strconv.Itoa(pid))
	}
	return args
}

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

	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

const (
	CoordinatorBinaryName        = "cuinterpose-coordinator"
	DefaultCoordinatorBinaryPath = "/usr/local/bin/" + CoordinatorBinaryName
)

func RemoveStaleCuinterposeSockets(controlDir string, namespacePIDs []int) error {
	for _, pid := range namespacePIDs {
		path := filepath.Join(controlDir, fmt.Sprintf("cuinterpose-%d.sock", pid))
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// Prepare tears down shared mappings; the caller must terminate the source on failure.
func PrepareCuinterpose(ctx context.Context, checkpointDir, procRoot string, targetPID int, namespacePIDs []int, binary string) error {
	processDir := filepath.Join(procRoot, strconv.Itoa(targetPID))
	mountNS, err := os.Open(filepath.Join(processDir, "ns/mnt"))
	if err != nil {
		return err
	}
	defer mountNS.Close()
	checkpoint, err := os.Open(checkpointDir)
	if err != nil {
		return err
	}
	defer checkpoint.Close()
	cmd, closeFiles, err := snapshotruntime.CommandInNamespaces(ctx, targetPID, mountNS, filepath.Join(processDir, "root"), binary)
	if err != nil {
		return err
	}
	defer closeFiles()
	args := cuinterposeArgs("prepare", snapshotruntime.InheritFile(cmd, checkpoint), namespacePIDs)
	cmd.Args = append(cmd.Args, args...)
	return executeCoordinator(cmd)
}

// Called inside the restored namespaces with a binary descriptor opened before CRIU.
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

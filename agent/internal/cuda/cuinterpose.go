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

func RemoveStaleCuInterposeSockets(controlDir string, namespacePIDs []int) error {
	for _, pid := range namespacePIDs {
		path := filepath.Join(controlDir, fmt.Sprintf("cuinterpose-%d.sock", pid))
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// Inspect only reads state and requires every participant to reply and pass topology
// validation before the caller enters the phase where failure requires source
// termination.
func InspectCuInterpose(ctx context.Context, procRoot string, targetPID int, namespacePIDs []int, binary string) error {
	return runCuInterposeInContainer(ctx, "inspect", "", procRoot, targetPID, namespacePIDs, binary)
}

// Prepare removes shared mappings, so the caller must terminate the source on failure.
func PrepareCuInterpose(ctx context.Context, checkpointDir, procRoot string, targetPID int, namespacePIDs []int, binary string) error {
	return runCuInterposeInContainer(ctx, "prepare", checkpointDir, procRoot, targetPID, namespacePIDs, binary)
}

func runCuInterposeInContainer(ctx context.Context, operation, checkpointDir, procRoot string, targetPID int, namespacePIDs []int, binary string) error {
	processDir := filepath.Join(procRoot, strconv.Itoa(targetPID))
	mountNS, err := os.Open(filepath.Join(processDir, "ns/mnt"))
	if err != nil {
		return err
	}
	defer mountNS.Close()
	cmd, closeFiles, err := snapshotruntime.CommandInNamespaces(ctx, targetPID, mountNS, filepath.Join(processDir, "root"), binary)
	if err != nil {
		return err
	}
	defer closeFiles()
	if checkpointDir != "" {
		checkpoint, err := os.Open(checkpointDir)
		if err != nil {
			return err
		}
		defer checkpoint.Close()
		checkpointDir = snapshotruntime.InheritFile(cmd, checkpoint)
	}
	args := cuInterposeArgs(operation, checkpointDir, namespacePIDs)
	cmd.Args = append(cmd.Args, args...)
	return executeCoordinator(cmd)
}

// This runs inside the restored namespaces through a binary descriptor opened before
// CRIU.
func RestoreCuInterpose(ctx context.Context, checkpointDir string, namespacePIDs []int, binary string) error {
	// Sharing nsrestore's process group lets cancellation by its host parent reach this
	// coordinator even if nsrestore has already been killed.
	return executeCoordinator(exec.CommandContext(ctx, binary, cuInterposeArgs("restore", checkpointDir, namespacePIDs)...))
}

func executeCoordinator(cmd *exec.Cmd) error {
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cuinterpose coordinator: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func cuInterposeArgs(operation, checkpointDir string, namespacePIDs []int) []string {
	args := []string{"--" + operation}
	if checkpointDir != "" {
		args = append(args, "--checkpoint-dir", checkpointDir)
	}
	args = append(args, "--control-dir", podcontract.SnapshotControlMountPath)
	for _, pid := range namespacePIDs {
		args = append(args, "--process", strconv.Itoa(pid))
	}
	return args
}

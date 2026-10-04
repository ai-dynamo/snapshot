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

type CuInterposeTarget struct {
	ProcRoot      string
	TargetPID     int
	NamespacePIDs []int
	Binary        string
}

type cuInterposeOperation string

const (
	cuInterposeInspect cuInterposeOperation = "inspect"
	cuInterposePrepare cuInterposeOperation = "prepare"
	cuInterposeRestore cuInterposeOperation = "restore"
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
func InspectCuInterpose(ctx context.Context, target CuInterposeTarget) error {
	return runCuInterposeInContainer(ctx, cuInterposeInspect, target, "")
}

// Prepare removes shared mappings, so the caller must terminate the source on failure.
func PrepareCuInterpose(ctx context.Context, target CuInterposeTarget, checkpointDir string) error {
	return runCuInterposeInContainer(ctx, cuInterposePrepare, target, checkpointDir)
}

func runCuInterposeInContainer(ctx context.Context, operation cuInterposeOperation, target CuInterposeTarget, checkpointDir string) error {
	processDir := filepath.Join(target.ProcRoot, strconv.Itoa(target.TargetPID))
	mountNS, err := os.Open(filepath.Join(processDir, "ns/mnt"))
	if err != nil {
		return fmt.Errorf("cuinterpose %s: open mount namespace for process %d: %w", operation, target.TargetPID, err)
	}
	defer mountNS.Close()
	cmd, closeFiles, err := snapshotruntime.CommandInNamespaces(ctx, target.TargetPID, mountNS, filepath.Join(processDir, "root"), target.Binary)
	if err != nil {
		return fmt.Errorf("cuinterpose %s: prepare namespace command %s for process %d: %w", operation, target.Binary, target.TargetPID, err)
	}
	defer closeFiles()
	if checkpointDir != "" {
		checkpoint, err := os.Open(checkpointDir)
		if err != nil {
			return fmt.Errorf("cuinterpose %s: open checkpoint directory for process %d: %w", operation, target.TargetPID, err)
		}
		defer checkpoint.Close()
		checkpointDir = snapshotruntime.InheritFile(cmd, checkpoint)
	}
	args := cuInterposeArgs(operation, checkpointDir, target.NamespacePIDs)
	cmd.Args = append(cmd.Args, args...)
	return executeCoordinator(cmd)
}

// This runs inside the restored namespaces through a binary descriptor opened before
// CRIU.
func RestoreCuInterpose(ctx context.Context, checkpointDir string, namespacePIDs []int, binary string) error {
	// Sharing nsrestore's process group lets cancellation by its host parent reach this
	// coordinator even if nsrestore has already been killed.
	return executeCoordinator(exec.CommandContext(ctx, binary, cuInterposeArgs(cuInterposeRestore, checkpointDir, namespacePIDs)...))
}

func executeCoordinator(cmd *exec.Cmd) error {
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cuinterpose coordinator: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func cuInterposeArgs(operation cuInterposeOperation, checkpointDir string, namespacePIDs []int) []string {
	args := []string{"--" + string(operation)}
	if checkpointDir != "" {
		args = append(args, "--checkpoint-dir", checkpointDir)
	}
	args = append(args, "--control-dir", podcontract.SnapshotControlMountPath)
	for _, pid := range namespacePIDs {
		args = append(args, "--process", strconv.Itoa(pid))
	}
	return args
}

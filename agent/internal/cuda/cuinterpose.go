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
)

const (
	CoordinatorBinaryName        = "cuinterpose-coordinator"
	DefaultCoordinatorBinaryPath = "/usr/local/bin/" + CoordinatorBinaryName

	// The core binds one endpoint per process in the workload's /tmp, named by
	// its namespace PID.
	cuInterposeSocketDir     = "/tmp"
	CuInterposeSocketPattern = cuInterposeSocketDir + "/cuinterpose-*.sock"
)

// CuInterposeTarget selects the source container a coordinator command runs in.
type CuInterposeTarget struct {
	ProcRoot string
	// HostPID indexes ProcRoot and selects the namespaces the coordinator enters.
	HostPID int
	// NamespacePIDs are the participants' innermost namespace PIDs, which name their
	// coordinator endpoints.
	NamespacePIDs []int
	Binary        string
}

type cuInterposeOperation string

const (
	cuInterposeInspect cuInterposeOperation = "inspect"
	cuInterposePrepare cuInterposeOperation = "prepare"
	cuInterposeRestore cuInterposeOperation = "restore"
)

// InspectCuInterpose only reads state and requires every participant to reply and pass
// topology validation before the caller enters the phase where failure requires source
// termination.
func InspectCuInterpose(ctx context.Context, target CuInterposeTarget) error {
	return runCuInterposeInContainer(ctx, cuInterposeInspect, target, "")
}

// PrepareCuInterpose removes shared mappings, so the caller must terminate the source on
// failure.
func PrepareCuInterpose(ctx context.Context, target CuInterposeTarget, checkpointDir string) error {
	return runCuInterposeInContainer(ctx, cuInterposePrepare, target, checkpointDir)
}

func runCuInterposeInContainer(ctx context.Context, operation cuInterposeOperation, target CuInterposeTarget, checkpointDir string) error {
	processDir := filepath.Join(target.ProcRoot, strconv.Itoa(target.HostPID))
	mountNS, err := os.Open(filepath.Join(processDir, "ns/mnt"))
	if err != nil {
		return fmt.Errorf("cuinterpose %s: open mount namespace for process %d: %w", operation, target.HostPID, err)
	}
	defer mountNS.Close()
	cmd, closeFiles, err := snapshotruntime.CommandInNamespaces(ctx, target.HostPID, mountNS, filepath.Join(processDir, "root"), target.Binary)
	if err != nil {
		return fmt.Errorf("cuinterpose %s: prepare namespace command %s for process %d: %w", operation, target.Binary, target.HostPID, err)
	}
	defer closeFiles()
	if checkpointDir != "" {
		checkpoint, err := os.Open(checkpointDir)
		if err != nil {
			return fmt.Errorf("cuinterpose %s: open checkpoint directory for process %d: %w", operation, target.HostPID, err)
		}
		defer checkpoint.Close()
		checkpointDir = snapshotruntime.InheritFile(cmd, checkpoint)
	}
	command := coordinatorCommand{operation: operation, checkpointDir: checkpointDir, namespacePIDs: target.NamespacePIDs}
	cmd.Args = append(cmd.Args, command.args()...)
	return executeCoordinator(cmd)
}

// RestoreCuInterpose runs inside the restored namespaces through a binary descriptor
// opened before CRIU.
func RestoreCuInterpose(ctx context.Context, checkpointDir string, namespacePIDs []int, binary string) error {
	// Sharing nsrestore's process group lets cancellation by its host parent reach this
	// coordinator even if nsrestore has already been killed.
	command := coordinatorCommand{operation: cuInterposeRestore, checkpointDir: checkpointDir, namespacePIDs: namespacePIDs}
	return executeCoordinator(exec.CommandContext(ctx, binary, command.args()...))
}

func executeCoordinator(cmd *exec.Cmd) error {
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cuinterpose coordinator: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// coordinatorCommand describes one coordinator invocation.
type coordinatorCommand struct {
	operation     cuInterposeOperation
	checkpointDir string
	namespacePIDs []int
}

func (c coordinatorCommand) args() []string {
	args := []string{"--" + string(c.operation)}
	if c.checkpointDir != "" {
		args = append(args, "--checkpoint-dir", c.checkpointDir)
	}
	args = append(args, "--socket-dir", cuInterposeSocketDir)
	for _, pid := range c.namespacePIDs {
		args = append(args, "--process", strconv.Itoa(pid))
	}
	return args
}

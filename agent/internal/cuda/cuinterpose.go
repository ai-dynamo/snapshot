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

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/go-logr/logr"
)

const (
	cuinterposerCoordinator  = "/usr/local/bin/cuinterposer-coordinator"
	cuinterposerSocketPrefix = "cuinterposer-"
	cuinterposerStateFile    = "cuinterposer.state"
)

func snapshotControlDir() string {
	return strings.TrimPrefix(snapshotv1alpha1.SnapshotControlMountPath, string(os.PathSeparator))
}

func cuinterposerEndpointPath(procRoot string, observedPID, namespacePID int) string {
	return filepath.Join(
		procRoot,
		strconv.Itoa(observedPID),
		"root",
		snapshotControlDir(),
		fmt.Sprintf("%s%d.sock", cuinterposerSocketPrefix, namespacePID),
	)
}

// DetectCUDAInterposition reports whether the live CUDA processes are running
// the interposer. The signal is the shim's Unix sockets under the container's
// snapshot-control mount, not /proc/<pid>/environ: Python setproctitle
// (vLLM/SGLang) can clobber procfs environment while the sockets remain.
// No sockets skips prepare. A partial or invalid set fails closed.
func DetectCUDAInterposition(procRoot string, observedPIDs, namespacePIDs []int) (bool, error) {
	if len(observedPIDs) != len(namespacePIDs) {
		return false, fmt.Errorf(
			"cuinterposer PID mapping count mismatch: observed=%d namespace=%d",
			len(observedPIDs),
			len(namespacePIDs),
		)
	}
	if len(observedPIDs) == 0 {
		return false, nil
	}
	validEndpoints := 0
	seenEndpoints := 0
	for index, observedPID := range observedPIDs {
		endpoint := cuinterposerEndpointPath(procRoot, observedPID, namespacePIDs[index])
		info, err := os.Lstat(endpoint)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("stat cuinterposer endpoint %q: %w", endpoint, err)
		}
		seenEndpoints++
		if info.Mode()&os.ModeSocket != 0 {
			validEndpoints++
		}
	}
	if seenEndpoints == 0 {
		return false, nil
	}
	if validEndpoints != len(observedPIDs) {
		return false, fmt.Errorf(
			"cuinterposer endpoint missing or invalid for %d of %d CUDA processes",
			len(observedPIDs)-validEndpoints,
			len(observedPIDs),
		)
	}
	return true, nil
}

func HasCUDAInterpositionState(checkpointDir string) (bool, error) {
	_, err := os.Stat(filepath.Join(checkpointDir, cuinterposerStateFile))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func PrepareCUDAInterposition(
	ctx context.Context,
	checkpointDir string,
	procRoot string,
	observedPIDs []int,
	namespacePIDs []int,
	log logr.Logger,
) error {
	args, err := cuinterposerArgs("prepare", checkpointDir, procRoot, observedPIDs, namespacePIDs)
	if err != nil {
		return err
	}
	output, err := exec.CommandContext(ctx, cuinterposerCoordinator, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s failed: %w (output: %s)", cuinterposerCoordinator, err, strings.TrimSpace(string(output)))
	}
	log.Info("Prepared cuinterposer state")
	return nil
}

func RestoreCUDAInterposition(
	ctx context.Context,
	checkpointDir string,
	observedPIDs []int,
	namespacePIDs []int,
) error {
	args, err := cuinterposerArgs("restore", checkpointDir, "", observedPIDs, namespacePIDs)
	if err != nil {
		return err
	}
	output, err := exec.CommandContext(ctx, cuinterposerCoordinator, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s failed: %w (output: %s)", cuinterposerCoordinator, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func cuinterposerArgs(operation, checkpointDir, procRoot string, observedPIDs, namespacePIDs []int) ([]string, error) {
	if len(observedPIDs) != len(namespacePIDs) {
		return nil, fmt.Errorf(
			"cuinterposer PID mapping count mismatch: observed=%d namespace=%d",
			len(observedPIDs),
			len(namespacePIDs),
		)
	}
	args := []string{
		"--" + operation,
		"--proc-root",
		procRoot,
		"--checkpoint-dir",
		checkpointDir,
	}
	for index, observedPID := range observedPIDs {
		args = append(
			args,
			"--process",
			strconv.Itoa(observedPID),
			strconv.Itoa(namespacePIDs[index]),
		)
	}
	return args, nil
}

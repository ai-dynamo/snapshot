// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// cuinterpose is the CUDA interposer shim (agent/cmd/cuinterpose). Each CUDA
// process running it listens on a Unix socket under the pod's snapshot control
// directory. The agent never talks to those sockets itself; it runs the
// cuinterpose-coordinator binary, which does, once before the native CUDA
// checkpoint (prepare) and once after the native CUDA restore (restore).
//
// The string constants below mirror the Rust core and coordinator; a test
// checks their endpoint and command-line contracts agree.
const (
	// CoordinatorBinaryName is the cuinterpose-coordinator executable name.
	CoordinatorBinaryName = "cuinterpose-coordinator"
	// DefaultCoordinatorBinaryPath is where the agent image installs the
	// coordinator, used for prepare. Restore runs inside the restored
	// container's mount namespace, which does not contain the agent bundle, so
	// that call site passes a /proc/self/fd path opened before CRIU ran.
	DefaultCoordinatorBinaryPath = "/usr/local/bin/" + CoordinatorBinaryName
	// CuinterposeStateFile is the topology sidecar the coordinator writes into
	// the checkpoint directory during prepare and reads during restore.
	CuinterposeStateFile = "cuinterpose.state"

	cuinterposeSocketPrefix = "cuinterpose-"
	cuinterposeSocketSuffix = ".sock"
)

// cuinterposeEndpointPath is the shim's control socket for one CUDA process,
// reached through the host's /proc mount: <procRoot>/<pid>/root is the
// process's own root filesystem.
func cuinterposeEndpointPath(procRoot string, observedPID, namespacePID int) string {
	return filepath.Join(
		procRoot,
		strconv.Itoa(observedPID),
		"root",
		strings.TrimPrefix(podcontract.SnapshotControlMountPath, string(os.PathSeparator)),
		cuinterposeSocketName(namespacePID),
	)
}

func cuinterposeSocketName(namespacePID int) string {
	return fmt.Sprintf("%s%d%s", cuinterposeSocketPrefix, namespacePID, cuinterposeSocketSuffix)
}

// DetectCuinterpose reports whether the live CUDA processes run the shim. The
// signal is the shim's control socket, one per CUDA process, not the process
// environment: Python's setproctitle (vLLM, SGLang) overwrites what /proc
// shows as the environment while the sockets remain. No sockets at all means
// the workload is not interposed and is checkpointed natively. Some but not
// all sockets, or a non-socket file in a socket's place, is an error: a
// half-interposed process tree cannot be checkpointed consistently.
func DetectCuinterpose(procRoot string, observedPIDs, namespacePIDs []int) (bool, error) {
	if len(observedPIDs) != len(namespacePIDs) {
		return false, fmt.Errorf(
			"cuinterpose PID mapping count mismatch: observed=%d namespace=%d",
			len(observedPIDs),
			len(namespacePIDs),
		)
	}
	if len(namespacePIDs) == 0 {
		return false, nil
	}
	valid := 0
	seen := 0
	for index, observedPID := range observedPIDs {
		endpoint := cuinterposeEndpointPath(procRoot, observedPID, namespacePIDs[index])
		info, err := os.Lstat(endpoint)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("stat cuinterpose endpoint %q: %w", endpoint, err)
		}
		seen++
		if info.Mode()&os.ModeSocket != 0 {
			valid++
		}
	}
	if seen == 0 {
		return false, nil
	}
	if valid != len(observedPIDs) {
		return false, fmt.Errorf(
			"cuinterpose endpoint missing or invalid for %d of %d CUDA processes",
			len(observedPIDs)-valid,
			len(observedPIDs),
		)
	}
	return true, nil
}

// CheckCuinterposeEnablement is the rule for what the Pod asked for versus what
// the CUDA processes are running. Both disagreements are refused:
//
//   - requested but no process exposes a socket: the shim never loaded (wrong
//     path, glibc too old, LD_PRELOAD stripped). A native checkpoint would look
//     fine and restore with stale sharing, so it must not be taken.
//   - not requested but sockets are present: something other than Snapshot
//     preloaded the shim; the restore side would not mount it.
//
// With no CUDA processes there is nothing to interpose and nothing to check.
func CheckCuinterposeEnablement(requested, detected bool, cudaProcesses int) error {
	if cudaProcesses == 0 {
		return nil
	}
	if requested && !detected {
		return fmt.Errorf(
			"cuinterpose was requested (%s) but no CUDA process exposes a control socket; the shim did not load, refusing to checkpoint without it",
			podcontract.CuinterposeAnnotation)
	}
	if !requested && detected {
		return fmt.Errorf(
			"CUDA processes run the cuinterpose shim but the source Pod did not request it (%s); restore would not mount the shim",
			podcontract.CuinterposeAnnotation)
	}
	return nil
}

// RemoveStaleCuinterposeSockets deletes leftover shim sockets from an earlier
// incarnation of the pod. Each shim binds its socket by namespace PID, and
// CRIU recreates the checkpointed process with the same namespace PID, so a
// stale file at that path makes the restored bind() fail. Called inside the
// container's mount namespace before CRIU runs. Returns how many were removed.
func RemoveStaleCuinterposeSockets(controlDir string, namespacePIDs []int) (int, error) {
	removed := 0
	for _, namespacePID := range namespacePIDs {
		if namespacePID <= 0 {
			return removed, fmt.Errorf("invalid cuinterpose namespace PID %d", namespacePID)
		}
		path := filepath.Join(controlDir, cuinterposeSocketName(namespacePID))
		if err := os.Remove(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return removed, fmt.Errorf("remove stale cuinterpose socket %s: %w", path, err)
		}
		removed++
	}
	return removed, nil
}

// PrepareCuinterpose runs the coordinator in the live target container's mount,
// UTS, IPC, network, and PID namespaces before the native CUDA checkpoint. The
// executable and checkpoint directory are opened by the agent first and passed
// through file descriptors, so neither depends on a path supplied by the
// workload. On success the coordinator has written CuinterposeStateFile into
// checkpointDir.
//
// There is no undo: once prepare has torn down shared mappings the source
// workload can only continue by being restored, so a later checkpoint failure
// is fail-stop for the source (the caller terminates it).
func PrepareCuinterpose(
	ctx context.Context,
	checkpointDir string,
	procRoot string,
	targetPID int,
	namespacePIDs []int,
	coordinatorBinaryPath string,
) error {
	cmd, closeFiles, err := prepareCoordinatorCommand(ctx, "prepare", checkpointDir, procRoot, targetPID, namespacePIDs, coordinatorBinaryPath)
	if err != nil {
		return err
	}
	defer closeFiles()
	return executeCoordinator(cmd, coordinatorBinaryPath, "--prepare")
}

func prepareCoordinatorCommand(ctx context.Context, operation, checkpointDir, procRoot string, targetPID int, namespacePIDs []int, coordinatorBinaryPath string) (*exec.Cmd, func(), error) {
	if targetPID <= 0 {
		return nil, nil, fmt.Errorf("invalid cuinterpose target PID %d", targetPID)
	}

	const (
		binaryFD     = 3
		checkpointFD = 4
		mountNSFD    = 5
		utsNSFD      = 6
		ipcNSFD      = 7
		networkNSFD  = 8
		pidNSFD      = 9
		rootFD       = 10
	)
	args, err := cuinterposeArgs(
		operation,
		fmt.Sprintf("/proc/self/fd/%d", checkpointFD),
		podcontract.SnapshotControlMountPath,
		namespacePIDs,
	)
	if err != nil {
		return nil, nil, err
	}

	files := make([]*os.File, 0, 8)
	closeFiles := func() {
		for _, file := range files {
			_ = file.Close()
		}
	}
	for _, path := range []string{
		coordinatorBinaryPath,
		checkpointDir,
		filepath.Join(procRoot, strconv.Itoa(targetPID), "ns", "mnt"),
		filepath.Join(procRoot, strconv.Itoa(targetPID), "ns", "uts"),
		filepath.Join(procRoot, strconv.Itoa(targetPID), "ns", "ipc"),
		filepath.Join(procRoot, strconv.Itoa(targetPID), "ns", "net"),
		filepath.Join(procRoot, strconv.Itoa(targetPID), "ns", "pid"),
		filepath.Join(procRoot, strconv.Itoa(targetPID), "root"),
	} {
		file, err := os.Open(path)
		if err != nil {
			closeFiles()
			return nil, nil, fmt.Errorf("open cuinterpose prepare input %q: %w", path, err)
		}
		files = append(files, file)
	}

	nsenterArgs := []string{
		fmt.Sprintf("--mount=/proc/self/fd/%d", mountNSFD),
		fmt.Sprintf("--uts=/proc/self/fd/%d", utsNSFD),
		fmt.Sprintf("--ipc=/proc/self/fd/%d", ipcNSFD),
		fmt.Sprintf("--net=/proc/self/fd/%d", networkNSFD),
		fmt.Sprintf("--pid=/proc/self/fd/%d", pidNSFD),
		// setns(CLONE_NEWNS) alone does not replace fs.root or cwd.
		// Pin the target root before entry just like its namespace descriptors.
		fmt.Sprintf("--root=/proc/self/fd/%d", rootFD),
		fmt.Sprintf("--wd=/proc/self/fd/%d", rootFD),
		"--",
		fmt.Sprintf("/proc/self/fd/%d", binaryFD),
	}
	nsenterArgs = append(nsenterArgs, args...)
	cmd := exec.CommandContext(ctx, "nsenter", nsenterArgs...)
	cmd.ExtraFiles = files
	return cmd, closeFiles, nil
}

// RestoreCuinterpose runs from nsrestore, which already occupies the restored
// container's mount, UTS, IPC, network, and PID namespaces. procRoot is empty,
// so the control sockets are addressed directly under the control directory.
func RestoreCuinterpose(
	ctx context.Context,
	checkpointDir string,
	namespacePIDs []int,
	coordinatorBinaryPath string,
) error {
	args, err := cuinterposeArgs("restore", checkpointDir, podcontract.SnapshotControlMountPath, namespacePIDs)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, coordinatorBinaryPath, args...)
	return executeCoordinator(cmd, coordinatorBinaryPath, args[0])
}

func executeCoordinator(cmd *exec.Cmd, binary, operation string) error {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s failed: %w (stderr: %s)", binary, operation, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func cuinterposeArgs(operation, checkpointDir, controlDir string, namespacePIDs []int) ([]string, error) {
	if len(namespacePIDs) == 0 {
		return nil, errors.New("cuinterpose coordinator requires at least one CUDA process")
	}
	args := []string{
		"--" + operation,
		"--checkpoint-dir", checkpointDir,
		"--control-dir", controlDir,
	}
	for _, namespacePID := range namespacePIDs {
		if namespacePID <= 0 {
			return nil, fmt.Errorf("invalid cuinterpose namespace PID %d", namespacePID)
		}
		args = append(args, "--process", strconv.Itoa(namespacePID))
	}
	return args, nil
}

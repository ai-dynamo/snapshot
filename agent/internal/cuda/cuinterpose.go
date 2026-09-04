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

	"github.com/go-logr/logr"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// cuinterpose is the CUDA interposer shim (agent/cmd/cuinterpose). Each CUDA
// process running it listens on a Unix socket under the pod's snapshot control
// directory. The agent never talks to those sockets itself; it runs the
// cuinterpose-coordinator binary, which does, once before the native CUDA
// checkpoint (prepare) and once after the native CUDA restore (restore).
//
// The string constants below mirror agent/cmd/cuinterpose/protocol.h; a test
// checks they agree.
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
	coordinatorReportPrefix = "cuinterpose-coordinator "
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
	if len(observedPIDs) == 0 {
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

// HasCuinterposeState reports whether the checkpoint directory holds the
// coordinator's state file.
func HasCuinterposeState(checkpointDir string) (bool, error) {
	_, err := os.Stat(filepath.Join(checkpointDir, CuinterposeStateFile))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// RemoveStaleCuinterposeSockets deletes leftover shim sockets from an earlier
// incarnation of the pod. Each shim binds its socket by namespace PID, and
// CRIU recreates the checkpointed process with the same namespace PID, so a
// stale file at that path makes the restored bind() fail. Called inside the
// container's mount namespace before CRIU runs. Returns how many were removed.
func RemoveStaleCuinterposeSockets(controlDir string) (int, error) {
	entries, err := os.ReadDir(controlDir)
	if err != nil {
		return 0, fmt.Errorf("list cuinterpose control directory %s: %w", controlDir, err)
	}
	removed := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, cuinterposeSocketPrefix) || !strings.HasSuffix(name, cuinterposeSocketSuffix) {
			continue
		}
		if err := os.Remove(filepath.Join(controlDir, name)); err != nil && !os.IsNotExist(err) {
			return removed, fmt.Errorf("remove stale cuinterpose socket %s: %w", name, err)
		}
		removed++
	}
	return removed, nil
}

// CoordinatorPhase is one progress line the coordinator printed:
// "cuinterpose-coordinator phase=<name> status=ok key=value ...".
type CoordinatorPhase struct {
	Phase  string
	Fields map[string]string
}

// PrepareCuinterpose runs the coordinator against the live CUDA processes,
// through the host's /proc mount, before the native CUDA checkpoint. On
// success the coordinator has written CuinterposeStateFile into checkpointDir.
// There is no undo: once prepare has torn down shared mappings the source
// workload can only continue by being restored, so a later checkpoint failure
// is fail-stop for the source (the caller terminates it).
func PrepareCuinterpose(
	ctx context.Context,
	checkpointDir string,
	procRoot string,
	observedPIDs []int,
	namespacePIDs []int,
	coordinatorBinaryPath string,
	log logr.Logger,
) ([]CoordinatorPhase, error) {
	args, err := cuinterposeArgs("prepare", checkpointDir, procRoot, podcontract.SnapshotControlMountPath, observedPIDs, namespacePIDs)
	if err != nil {
		return nil, err
	}
	return runCoordinator(ctx, coordinatorBinaryPath, args, log)
}

// RestoreCuinterpose runs the coordinator inside the restored container's
// mount namespace after the native CUDA restore, so procRoot is empty and the
// control sockets are addressed directly under the control directory.
func RestoreCuinterpose(
	ctx context.Context,
	checkpointDir string,
	observedPIDs []int,
	namespacePIDs []int,
	coordinatorBinaryPath string,
	log logr.Logger,
) ([]CoordinatorPhase, error) {
	args, err := cuinterposeArgs("restore", checkpointDir, "", podcontract.SnapshotControlMountPath, observedPIDs, namespacePIDs)
	if err != nil {
		return nil, err
	}
	return runCoordinator(ctx, coordinatorBinaryPath, args, log)
}

func runCoordinator(ctx context.Context, binary string, args []string, log logr.Logger) ([]CoordinatorPhase, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	phases := parseCoordinatorReports(stdout.String())
	for _, phase := range phases {
		values := make([]any, 0, 2+2*len(phase.Fields))
		values = append(values, "operation", args[0], "phase", phase.Phase)
		for key, value := range phase.Fields {
			values = append(values, key, value)
		}
		log.Info("cuinterpose coordinator phase", values...)
	}
	if runErr != nil {
		completed := make([]string, 0, len(phases))
		for _, phase := range phases {
			completed = append(completed, phase.Phase)
		}
		return phases, fmt.Errorf(
			"%s %s failed: %w (completed phases: %s; stderr: %s)",
			binary, args[0], runErr,
			strings.Join(completed, ","),
			strings.TrimSpace(stderr.String()),
		)
	}
	return phases, nil
}

// parseCoordinatorReports extracts progress lines from the coordinator's
// stdout; anything else on stdout is ignored.
func parseCoordinatorReports(output string) []CoordinatorPhase {
	var phases []CoordinatorPhase
	for _, line := range strings.Split(output, "\n") {
		if phase, ok := parseCoordinatorReport(line); ok {
			phases = append(phases, phase)
		}
	}
	return phases
}

func parseCoordinatorReport(line string) (CoordinatorPhase, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, coordinatorReportPrefix) {
		return CoordinatorPhase{}, false
	}
	phase := CoordinatorPhase{Fields: map[string]string{}}
	for _, field := range strings.Fields(strings.TrimPrefix(line, coordinatorReportPrefix)) {
		key, value, found := strings.Cut(field, "=")
		if !found {
			continue
		}
		if key == "phase" {
			phase.Phase = value
			continue
		}
		phase.Fields[key] = value
	}
	if phase.Phase == "" {
		return CoordinatorPhase{}, false
	}
	return phase, true
}

func cuinterposeArgs(operation, checkpointDir, procRoot, controlDir string, observedPIDs, namespacePIDs []int) ([]string, error) {
	if len(observedPIDs) != len(namespacePIDs) {
		return nil, fmt.Errorf(
			"cuinterpose PID mapping count mismatch: observed=%d namespace=%d",
			len(observedPIDs),
			len(namespacePIDs),
		)
	}
	if len(observedPIDs) == 0 {
		return nil, errors.New("cuinterpose coordinator requires at least one CUDA process")
	}
	args := []string{
		"--" + operation,
		"--proc-root", procRoot,
		"--checkpoint-dir", checkpointDir,
		"--control-dir", controlDir,
	}
	for index, observedPID := range observedPIDs {
		args = append(args, "--process", strconv.Itoa(observedPID), strconv.Itoa(namespacePIDs[index]))
	}
	return args, nil
}

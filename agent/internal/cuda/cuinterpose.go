// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/sys/unix"

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

// RestorePipelined overlaps only PageBroker allocation loading with the next
// process's native restore. Native restores must remain serial: CUDA's launch-job
// file is shared. Workload threads must remain parked behind restore-complete.
// On failure the coordinator drains started loads before this operation returns.
func RestorePipelined(ctx context.Context, checkpointDir string, observedPIDs, namespacePIDs []int, deviceMap, helperPath, coordinatorPath string, sessions AllocationSessions, log logr.Logger) error {
	args, err := cuinterposeArgs("restore", checkpointDir, "", podcontract.SnapshotControlMountPath, observedPIDs, namespacePIDs)
	if err != nil {
		return err
	}
	executable, err := os.Open(coordinatorPath)
	if err != nil {
		return err
	}
	defer executable.Close()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	parent := os.NewFile(uintptr(fds[0]), "restore-ready-parent")
	child := os.NewFile(uintptr(fds[1]), "restore-ready-child")
	defer child.Close()
	connection, err := net.FileConn(parent)
	_ = parent.Close()
	if err != nil {
		return err
	}
	defer connection.Close()
	socket := connection.(*net.UnixConn)
	cmd := exec.CommandContext(ctx, "/proc/self/fd/3", args...)
	cmd.ExtraFiles = []*os.File{executable, child}
	cmd.Args = append(cmd.Args, "--restore-ready-fd", "4")
	sessions.AppendTo(cmd)
	return runPipelinedRestore(ctx, cmd, socket, observedPIDs, func(ctx context.Context, pid int) error {
		if err := restoreProcess(ctx, pid, deviceMap, helperPath, log); err != nil {
			return err
		}
		if err := unlock(ctx, pid, helperPath, log); err != nil {
			state, stateErr := getState(ctx, pid, helperPath)
			if stateErr != nil || state != "running" {
				return err
			}
			log.Info("cuda-checkpoint-helper unlock returned error but process is already running", "pid", pid)
		}
		return nil
	}, log)
}

func runPipelinedRestore(ctx context.Context, cmd *exec.Cmd, socket *net.UnixConn, pids []int, restore func(context.Context, int) error, log logr.Logger) error {
	nativeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	start := time.Now()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	// Only the child may retain the readiness endpoint. Keeping our copy open
	// would hide coordinator death from the native-restore loop.
	_ = cmd.ExtraFiles[1].Close()
	done := make(chan error, 1)
	go func() {
		_, err := coordinatorResult(cmd.Wait(), stdout.String(), stderr.String(), cmd.Path, "--restore", log)
		if err != nil {
			cancel()
			_ = socket.Close()
		}
		done <- err
	}()
	nativeErr := func() error {
		// A missing peer or invalid topology must fail before touching CUDA.
		if err := socket.SetDeadline(time.Now().Add(10 * time.Minute)); err != nil {
			return err
		}
		var message [4]byte
		if _, err := io.ReadFull(socket, message[:]); err != nil {
			return fmt.Errorf("coordinator preflight: %w", err)
		}
		if binary.BigEndian.Uint32(message[:]) != 0 {
			return errors.New("invalid coordinator preflight acknowledgment")
		}
		go func() {
			var extra [1]byte
			_, _ = socket.Read(extra[:])
			cancel()
		}()
		for _, pid := range pids {
			if err := nativeCtx.Err(); err != nil {
				return err
			}
			if err := restore(nativeCtx, pid); err != nil {
				return err
			}
			binary.BigEndian.PutUint32(message[:], uint32(pid))
			if _, err := socket.Write(message[:]); err != nil {
				return fmt.Errorf("notify ready PID %d: %w", pid, err)
			}
		}
		return nil
	}()
	// EOF is the end-of-native barrier. On an incomplete sequence the
	// coordinator refuses topology replay but joins all started exchanges.
	_ = socket.CloseWrite()
	err := errors.Join(nativeErr, <-done)
	log.Info("CUDA restore pipeline completed", "duration", time.Since(start), "succeeded", err == nil)
	return err
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

// CoordinatorPhase is one JSON progress report from the coordinator.
type CoordinatorPhase struct {
	PID                            int      `json:"pid,omitempty"`
	UnixMS                         uint64   `json:"unix_ms,omitempty"`
	Succeeded                      *bool    `json:"succeeded,omitempty"`
	Phase                          string   `json:"phase"`
	Status                         string   `json:"status"`
	ElapsedMS                      float64  `json:"elapsed_ms"`
	Participants                   uint64   `json:"participants"`
	Records                        *uint64  `json:"records,omitempty"`
	LiveRawImports                 *uint64  `json:"live_raw_imports,omitempty"`
	UnsupportedExportableCreations *uint64  `json:"unsupported_exportable_creations,omitempty"`
	AllocationCount                *uint64  `json:"allocation_count,omitempty"`
	AllocationBytes                *uint64  `json:"allocation_bytes,omitempty"`
	GBPerS                         *float64 `json:"gb_per_s,omitempty"`
	CopyGBPerS                     *float64 `json:"copy_gb_per_s,omitempty"`
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
	observedPIDs []int,
	namespacePIDs []int,
	coordinatorBinaryPath string,
	log logr.Logger,
	sessions ...AllocationSessions,
) ([]CoordinatorPhase, error) {
	cmd, closeFiles, err := prepareCoordinatorCommand(ctx, "prepare", checkpointDir, procRoot, targetPID, observedPIDs, namespacePIDs, coordinatorBinaryPath)
	if err != nil {
		return nil, err
	}
	defer closeFiles()
	if len(sessions) != 0 {
		sessions[0].AppendTo(cmd)
	}
	return executeCoordinator(cmd, coordinatorBinaryPath, "--prepare", log)
}

// IdentifyCuinterpose validates live topology without mutation in the same
// pinned namespace environment as prepare.
func IdentifyCuinterpose(ctx context.Context, checkpointDir, procRoot string, targetPID int, observedPIDs, namespacePIDs []int, binary string) ([]string, error) {
	cmd, closeFiles, err := prepareCoordinatorCommand(ctx, "identify", checkpointDir, procRoot, targetPID, observedPIDs, namespacePIDs, binary)
	if err != nil {
		return nil, err
	}
	defer closeFiles()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("identify cuinterpose: %w: %s", err, stderr.String())
	}
	var ids []string
	if err := json.Unmarshal(output, &ids); err != nil {
		return nil, err
	}
	if len(ids) != len(namespacePIDs) {
		return nil, fmt.Errorf("cuinterpose participant/PID count differs")
	}
	return ids, nil
}

func prepareCoordinatorCommand(ctx context.Context, operation, checkpointDir, procRoot string, targetPID int, observedPIDs, namespacePIDs []int, coordinatorBinaryPath string) (*exec.Cmd, func(), error) {
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
		"",
		podcontract.SnapshotControlMountPath,
		observedPIDs,
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
	observedPIDs []int,
	namespacePIDs []int,
	coordinatorBinaryPath string,
	log logr.Logger,
	sessions ...AllocationSessions,
) ([]CoordinatorPhase, error) {
	args, err := cuinterposeArgs("restore", checkpointDir, "", podcontract.SnapshotControlMountPath, observedPIDs, namespacePIDs)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, coordinatorBinaryPath, args...)
	if len(sessions) != 0 {
		// Pin the executable into its own child slot before appending sessions:
		// nsrestore's /proc/self/fd/N may otherwise be overwritten by ExtraFiles.
		binary, err := os.Open(coordinatorBinaryPath)
		if err != nil {
			return nil, err
		}
		defer binary.Close()
		cmd.Path = "/proc/self/fd/3"
		cmd.Args[0] = cmd.Path
		cmd.ExtraFiles = []*os.File{binary}
		sessions[0].AppendTo(cmd)
	}
	return executeCoordinator(cmd, coordinatorBinaryPath, args[0], log)
}

func executeCoordinator(cmd *exec.Cmd, binary, operation string, log logr.Logger) ([]CoordinatorPhase, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	return coordinatorResult(runErr, stdout.String(), stderr.String(), binary, operation, log)
}

func coordinatorResult(runErr error, stdout, stderr, binary, operation string, log logr.Logger) ([]CoordinatorPhase, error) {
	phases := parseCoordinatorReports(stdout)
	for _, phase := range phases {
		log.Info("cuinterpose coordinator phase", "operation", operation, "report", phase)
	}
	if runErr != nil {
		completed := make([]string, 0, len(phases))
		for _, phase := range phases {
			completed = append(completed, phase.Phase)
		}
		return phases, fmt.Errorf(
			"%s %s failed: %w (completed phases: %s; stderr: %s)",
			binary, operation, runErr,
			strings.Join(completed, ","),
			strings.TrimSpace(stderr),
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
	var phase CoordinatorPhase
	if err := json.Unmarshal([]byte(line), &phase); err != nil || phase.Phase == "" || phase.Status != "ok" {
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

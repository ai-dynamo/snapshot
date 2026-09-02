/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

package cuda

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"

	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

const (
	daemonProtocolMagic   = uint32(0x50484344)
	daemonProtocolVersion = uint16(6)
	daemonRequestHeader   = 56
	daemonResponseHeader  = 24
	daemonMaxRequest      = 64 * 1024
	daemonMaxResponse     = 128 * 1024
	daemonMaxCgroup       = 4096
	daemonMaxJobFile      = 4096

	daemonActionHealth     = uint16(0)
	daemonActionCheckpoint = uint16(1)
	daemonActionRestore    = uint16(2)
	daemonActionLock       = uint16(3)
	daemonActionUnlock     = uint16(4)

	daemonResponseFatal           = uint32(1 << 0)
	daemonCapabilityDeferredCUDA  = uint32(1 << 1)
	daemonCapabilityCustomStorage = uint32(1 << 2)
	daemonResponseLockNotAcquired = uint32(1 << 3)
	// Match the helper's chart startup probe (2 seconds * 150 attempts). The
	// helper and agent are regular containers and Kubernetes does not guarantee
	// their startup order.
	daemonHealthWait          = 5 * time.Minute
	daemonHealthRetryInterval = 100 * time.Millisecond
	// The daemon rejects watchdogs longer than one hour. Keep the client wait
	// five minutes longer so the helper, not the caller, resolves an operation
	// that has already reached CUDA.
	daemonRPCTimeout = time.Hour + 5*time.Minute
)

var errDaemonUnavailable = errors.New("CUDA helper daemon unavailable")
var errDaemonFatal = errors.New("CUDA helper daemon entered fatal state")
var errCheckpointLockNotAcquired = errors.New("CUDA checkpoint lock was not acquired before mutation")
var daemonSocketPath = types.CUDAHelperSocketPath

func daemonRequest(request helperAction) ([]byte, error) {
	pid := request.PID
	action := request.Action
	deviceMap := request.DeviceMap
	storageMode := request.StorageMode
	storageDir := request.StorageDir
	jobFile := request.JobFile
	selectedDevices := request.GPUUUIDs
	transfer := request.Transfer
	identity := request.Identity
	var daemonAction uint16
	switch action {
	case "":
		daemonAction = daemonActionHealth
	case actionCheckpoint:
		daemonAction = daemonActionCheckpoint
	case actionRestore:
		daemonAction = daemonActionRestore
	case actionLock:
		daemonAction = daemonActionLock
	case actionUnlock:
		daemonAction = daemonActionUnlock
	default:
		return nil, fmt.Errorf("action %q is not supported by CUDA helper daemon", action)
	}
	health := daemonAction == daemonActionHealth
	var backend uint16
	switch storageMode {
	case "":
	case types.CUDAStorageModeLegacy:
		backend = 1
	case types.CUDAStorageModePOSIX:
		backend = 2
	default:
		return nil, fmt.Errorf("unsupported CUDA storage mode %q", storageMode)
	}
	if health {
		pid = 0
		identity = snapshotruntime.ProcessDetails{}
		deviceMap = ""
		storageDir = ""
		jobFile = ""
		selectedDevices = nil
		transfer = types.CUDATransferSettings{}
		if backend != 0 {
			return nil, errors.New("CUDA helper daemon health request has a backend")
		}
	} else if pid <= 0 || identity.OutermostPID != pid || identity.StartTimeTicks == 0 ||
		identity.Cgroup == "" || len(identity.Cgroup) > daemonMaxCgroup {
		return nil, errors.New("invalid CUDA helper daemon process identity")
	}
	if daemonAction == daemonActionLock || daemonAction == daemonActionUnlock {
		if backend == 0 || deviceMap != "" || storageDir != "" || len(selectedDevices) != 0 {
			return nil, errors.New("CUDA helper daemon lock/unlock request has transfer arguments")
		}
		transfer = types.CUDATransferSettings{}
	} else if !health {
		if backend == 0 {
			return nil, errors.New("CUDA helper daemon checkpoint/restore request has no backend")
		}
		if daemonAction == daemonActionCheckpoint && deviceMap != "" {
			return nil, errors.New("CUDA helper daemon checkpoint request has a device map")
		}
		if backend == 1 {
			if storageDir != "" || len(selectedDevices) != 0 {
				return nil, errors.New("regular CUDA helper request has a storage directory")
			}
			transfer = types.CUDATransferSettings{}
		} else {
			if storageDir == "" || storageDir[0] != '/' || len(selectedDevices) == 0 {
				return nil, errors.New("invalid CUDA helper daemon POSIX storage directory")
			}
			if err := transfer.Validate(); err != nil {
				return nil, fmt.Errorf("invalid CUDA helper daemon transfer settings: %w", err)
			}
		}
	}
	seenDevices := make(map[string]struct{}, len(selectedDevices))
	for _, uuid := range selectedDevices {
		if !gpuUUIDPattern.MatchString(uuid) {
			return nil, fmt.Errorf("invalid selected CUDA device %q", uuid)
		}
		canonicalUUID := strings.ToLower(uuid)
		if _, duplicate := seenDevices[canonicalUUID]; duplicate {
			return nil, fmt.Errorf("duplicate selected CUDA device %q", uuid)
		}
		seenDevices[canonicalUUID] = struct{}{}
	}
	selectedDeviceList := strings.Join(selectedDevices, ",")
	for name, value := range map[string]string{
		"device map":        deviceMap,
		"storage directory": storageDir,
		"process cgroup":    identity.Cgroup,
		"job file":          jobFile,
		"selected devices":  selectedDeviceList,
	} {
		if strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("CUDA helper daemon %s contains NUL", name)
		}
	}
	if len(jobFile) > daemonMaxJobFile || (jobFile != "" && jobFile[0] != '/') {
		return nil, errors.New("invalid CUDA helper daemon job file")
	}
	if len(deviceMap)+len(storageDir)+len(identity.Cgroup)+len(jobFile)+len(selectedDeviceList) > daemonMaxRequest-daemonRequestHeader {
		return nil, errors.New("CUDA helper daemon request is too large")
	}
	packet := make([]byte, daemonRequestHeader+len(deviceMap)+len(storageDir)+len(identity.Cgroup)+len(jobFile)+len(selectedDeviceList))
	binary.LittleEndian.PutUint32(packet[0:4], daemonProtocolMagic)
	binary.LittleEndian.PutUint16(packet[4:6], daemonProtocolVersion)
	binary.LittleEndian.PutUint16(packet[6:8], daemonRequestHeader)
	binary.LittleEndian.PutUint16(packet[8:10], daemonAction)
	binary.LittleEndian.PutUint16(packet[10:12], backend)
	binary.LittleEndian.PutUint32(packet[12:16], uint32(pid))
	binary.LittleEndian.PutUint32(packet[16:20], uint32(transfer.BufferCount))
	binary.LittleEndian.PutUint64(packet[20:28], transfer.ChunkBytes)
	binary.LittleEndian.PutUint32(packet[28:32], uint32(len(deviceMap)))
	binary.LittleEndian.PutUint32(packet[32:36], uint32(len(storageDir)))
	binary.LittleEndian.PutUint32(packet[36:40], uint32(len(identity.Cgroup)))
	binary.LittleEndian.PutUint64(packet[40:48], identity.StartTimeTicks)
	binary.LittleEndian.PutUint32(packet[48:52], uint32(len(jobFile)))
	binary.LittleEndian.PutUint32(packet[52:56], uint32(len(selectedDeviceList)))
	copy(packet[daemonRequestHeader:], deviceMap)
	copy(packet[daemonRequestHeader+len(deviceMap):], storageDir)
	copy(packet[daemonRequestHeader+len(deviceMap)+len(storageDir):], identity.Cgroup)
	copy(packet[daemonRequestHeader+len(deviceMap)+len(storageDir)+len(identity.Cgroup):], jobFile)
	copy(packet[daemonRequestHeader+len(deviceMap)+len(storageDir)+len(identity.Cgroup)+len(jobFile):], selectedDeviceList)
	return packet, nil
}

func parseDaemonResponse(packet []byte) (int32, uint32, string, string, error) {
	if len(packet) < daemonResponseHeader || len(packet) > daemonMaxResponse {
		return 0, 0, "", "", fmt.Errorf("invalid CUDA helper daemon response size %d", len(packet))
	}
	if binary.LittleEndian.Uint32(packet[0:4]) != daemonProtocolMagic ||
		binary.LittleEndian.Uint16(packet[4:6]) != daemonProtocolVersion ||
		binary.LittleEndian.Uint16(packet[6:8]) != daemonResponseHeader {
		return 0, 0, "", "", errors.New("invalid CUDA helper daemon response header")
	}
	flags := binary.LittleEndian.Uint32(packet[12:16])
	if flags & ^(daemonResponseFatal|daemonCapabilityDeferredCUDA|daemonCapabilityCustomStorage|daemonResponseLockNotAcquired) != 0 {
		return 0, 0, "", "", errors.New("invalid CUDA helper daemon response flags")
	}
	outputSize := int(binary.LittleEndian.Uint32(packet[16:20]))
	errorSize := int(binary.LittleEndian.Uint32(packet[20:24]))
	if outputSize < 0 || errorSize < 0 || outputSize+errorSize != len(packet)-daemonResponseHeader {
		return 0, 0, "", "", errors.New("invalid CUDA helper daemon response payload lengths")
	}
	payload := packet[daemonResponseHeader:]
	return int32(binary.LittleEndian.Uint32(packet[8:12])),
		flags, string(payload[:outputSize]), string(payload[outputSize:]), nil
}

func daemonRPC(
	ctx context.Context,
	socket string,
	packet []byte,
) (int32, uint32, string, string, time.Duration, error) {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unixpacket", socket)
	if err != nil {
		return 0, 0, "", "", 0, fmt.Errorf("%w at %s: %v", errDaemonUnavailable, socket, err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline := time.Now().Add(daemonRPCTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return 0, 0, "", "", 0, fmt.Errorf("set CUDA helper daemon RPC deadline: %w", err)
	}

	start := time.Now()
	written, err := conn.Write(packet)
	if err != nil {
		return 0, 0, "", "", time.Since(start),
			fmt.Errorf("CUDA helper daemon request write failed; operation outcome is unknown and will not be replayed: %w", err)
	}
	if written != len(packet) {
		return 0, 0, "", "", time.Since(start),
			fmt.Errorf("CUDA helper daemon request write was short (%d of %d bytes); operation outcome is unknown and will not be replayed", written, len(packet))
	}
	response := make([]byte, daemonMaxResponse+1)
	read, err := conn.Read(response)
	rpcWall := time.Since(start)
	if err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) || ctx.Err() != nil {
			cause := ctx.Err()
			if cause == nil {
				cause = err
			}
			return 0, 0, "", "", rpcWall,
				fmt.Errorf("CUDA helper daemon RPC ended after %s; operation outcome is unknown and will not be replayed: %w", rpcWall, cause)
		}
		return 0, 0, "", "", rpcWall,
			fmt.Errorf("CUDA helper daemon disconnected after request; operation outcome is unknown and will not be replayed: %w", err)
	}
	if read > daemonMaxResponse {
		return 0, 0, "", "", rpcWall, fmt.Errorf("CUDA helper daemon response exceeded %d bytes", daemonMaxResponse)
	}
	status, flags, stdout, stderr, err := parseDaemonResponse(response[:read])
	if err != nil {
		return 0, 0, "", "", rpcWall, err
	}
	return status, flags, stdout, stderr, rpcWall, nil
}

func runDaemonAction(
	ctx context.Context,
	request helperAction,
	log logr.Logger,
) error {
	packet, err := daemonRequest(request)
	if err != nil {
		return err
	}
	pid := request.PID
	action := request.Action
	storageMode := request.StorageMode
	status, flags, stdout, stderr, rpcWall, err := daemonRPC(ctx, daemonSocketPath, packet)
	if err != nil {
		return err
	}
	output := stdout + stderr
	if status != 0 {
		if action == actionLock && flags&daemonResponseLockNotAcquired != 0 {
			classification := errCheckpointLockNotAcquired
			if flags&daemonResponseFatal != 0 {
				classification = errors.Join(classification, errDaemonFatal)
			}
			return fmt.Errorf("%w: pid %d after %s with CUDA status %d (output: %s)",
				classification, pid, rpcWall, status, output)
		}
		if flags&daemonResponseFatal != 0 {
			return fmt.Errorf("%w: CUDA helper daemon %s failed for pid %d after %s with CUDA status %d (output: %s)",
				errDaemonFatal, action, pid, rpcWall, status, output)
		}
		return fmt.Errorf("CUDA helper daemon %s failed for pid %d after %s with CUDA status %d (output: %s)",
			action, pid, rpcWall, status, output)
	}
	if action == actionLock || action == actionUnlock {
		log.V(1).Info("CUDA helper daemon action succeeded",
			"pid", pid,
			"action", action,
			"daemon_rpc_wall_duration", rpcWall,
			"output", output,
		)
		return nil
	}
	if storageMode == types.CUDAStorageModeLegacy {
		log.V(1).Info("CUDA helper daemon action succeeded",
			"pid", pid,
			"action", action,
			"backend", storageMode,
			"daemon_rpc_wall_duration", rpcWall,
			"output", output,
		)
		return nil
	}
	telemetry := parseCustomStorageTelemetry(output, rpcWall)
	values := []any{
		"pid", pid,
		"action", action,
		"transport", "daemon",
		"daemon_rpc_wall_duration", rpcWall,
		"helper_telemetry_status", telemetry.status,
	}
	if telemetry.status == "valid" {
		values = append(values, "helper_operation_to_telemetry_duration", telemetry.helperMainDuration)
	} else {
		values = append(values, "helper_telemetry_error", telemetry.err)
	}
	log.Info("CUDA custom-storage transfer succeeded", append(values, "output", output)...)
	return nil
}

// validateCUDAOperationBudget rejects a long-running CUDA sequence before its
// first driver call. Per-request validation in runDaemonAction is too late for
// checkpoint because the process has already been locked by then.
func validateCUDAOperationBudget(ctx context.Context, action string, targetCount int) error {
	if targetCount < 0 {
		return fmt.Errorf("CUDA helper %s target count must not be negative", action)
	}
	callsPerTarget := 1
	if action == actionCheckpoint {
		// Checkpoint locks each target before asking the driver to checkpoint it.
		// Both calls may consume the full daemon RPC timeout. Restore unlock is a
		// bounded terminal action and is deliberately allowed under a short
		// caller deadline, so only the restore call contributes to this preflight.
		callsPerTarget = 2
	}
	maxTargets := int((time.Duration(1<<63-1) / daemonRPCTimeout) / time.Duration(callsPerTarget))
	if targetCount > maxTargets {
		return fmt.Errorf("CUDA helper %s target count %d exceeds operation-budget capacity", action, targetCount)
	}
	required := time.Duration(targetCount) * time.Duration(callsPerTarget) * daemonRPCTimeout
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= required {
		return fmt.Errorf(
			"CUDA helper %s for %d target(s) requires more than %s of caller budget before state-changing work",
			action, targetCount, required,
		)
	}
	return nil
}

func daemonCapabilities(ctx context.Context) (uint32, error) {
	packet, err := daemonRequest(helperAction{})
	if err != nil {
		return 0, err
	}
	// Health has a separate listener so it cannot queue behind a long-running
	// operation. Both listeners are created and owned by the same daemon process;
	// any accept/poll failure on either listener is fatal and terminates the
	// daemon, so a surviving health listener cannot mask a failed operation loop.
	status, flags, _, stderr, _, err := daemonRPC(ctx, daemonSocketPath+".health", packet)
	if err != nil {
		return 0, err
	}
	if status != 0 || flags&daemonCapabilityDeferredCUDA == 0 {
		return 0, fmt.Errorf("CUDA helper daemon health/capability check failed: status=%d flags=%#x error=%s",
			status, flags, stderr)
	}
	return flags, nil
}

// SelectCUDAStorageMode applies the operator's checkpoint-creation policy.
// It chooses one artifact storage mode for the entire checkpoint before any
// CUDA process is locked. Restore selection is separate: it always follows the
// published manifest.
var waitForDaemon = WaitForDaemon

func SelectCUDAStorageMode(ctx context.Context, configuredMode string) (string, error) {
	if err := waitForDaemon(ctx, configuredMode); err != nil {
		return "", err
	}
	switch configuredMode {
	case types.CUDAStorageModeLegacy:
		return configuredMode, nil
	case types.CUDAStorageModePOSIX:
		return configuredMode, nil
	default:
		return "", fmt.Errorf("unsupported configured CUDA storage mode %q", configuredMode)
	}
}

// ValidateCUDAStorageMode rejects unsupported artifacts before rootfs or CRIU
// restore changes the placeholder.
func ValidateCUDAStorageMode(ctx context.Context, storageMode string) error {
	flags, err := daemonCapabilities(ctx)
	if err != nil {
		return err
	}
	return validateCUDAStorageModeCapabilities(storageMode, flags)
}

func validateCUDAStorageModeCapabilities(storageMode string, flags uint32) error {
	switch storageMode {
	case types.CUDAStorageModeLegacy:
		return nil
	case types.CUDAStorageModePOSIX:
		if flags&daemonCapabilityCustomStorage == 0 {
			return errors.New("CUDA POSIX CustomStorage artifact requires daemon CustomStorage capability")
		}
		return nil
	default:
		return fmt.Errorf("unsupported CUDA storage mode %q", storageMode)
	}
}

// WaitForDaemon waits for protocol-level health and every capability required
// by the configured checkpoint creation mode.
func WaitForDaemon(ctx context.Context, storageMode string) error {
	waitCtx, cancel := context.WithTimeout(ctx, daemonHealthWait)
	defer cancel()
	ticker := time.NewTicker(daemonHealthRetryInterval)
	defer ticker.Stop()
	var lastErr error
	for {
		flags, err := daemonCapabilities(waitCtx)
		if err == nil {
			err = validateCUDAStorageModeCapabilities(storageMode, flags)
		}
		if err == nil {
			return nil
		}
		lastErr = err
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("wait for CUDA helper daemon at %s: %w (last error: %v)", daemonSocketPath, waitCtx.Err(), lastErr)
		case <-ticker.C:
		}
	}
}

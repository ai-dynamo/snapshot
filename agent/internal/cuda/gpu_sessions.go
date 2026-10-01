// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/sys/unix"
)

// GPUSessions holds private helper connections keyed by captured PID. The
// parent must call Helper.Drain before releasing the checkpoint directory.
type GPUSessions map[string]*os.File

func (sessions GPUSessions) Close() {
	for pid, file := range sessions {
		if file != nil {
			_ = file.Close()
		}
		delete(sessions, pid)
	}
}

func (sessions GPUSessions) halfClose() {
	for _, file := range sessions {
		if file != nil {
			_ = unix.Shutdown(int(file.Fd()), unix.SHUT_WR)
		}
	}
}

// BindGPUSessions pins the target namespace and artifact directory and validates
// saved metadata before CRIU. The CLI owns the private daemon wire format.
func BindGPUSessions(ctx context.Context, helper *Helper, artifact string, containerPID int, pids []int, devices []string, deviceMap string, save, customStorage, enableChecksumDigest bool) (GPUSessions, error) {
	if containerPID <= 1 || containerPID > math.MaxInt32 {
		return nil, fmt.Errorf("invalid GPU container PID %d", containerPID)
	}
	seen := make(map[int]bool, len(pids))
	for _, pid := range pids {
		if pid <= 0 || pid > math.MaxInt32 || seen[pid] {
			return nil, fmt.Errorf("invalid or duplicate GPU PID %d", pid)
		}
		seen[pid] = true
	}
	if helper == nil {
		return nil, fmt.Errorf("persistent CUDA helper is not running")
	}
	if customStorage && !helper.customStorage {
		return nil, fmt.Errorf("CUDA helper does not support CustomStorage")
	}
	root, err := os.Open(artifact)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	direction, mode := "load", "driver"
	if save {
		direction = "save"
	}
	if customStorage {
		mode = "custom"
	}
	args := []string{"--bind-batch", "--control-fd", "3", "--directory-fd", "4",
		"--container-pid", strconv.Itoa(containerPID), "--direction", direction, "--storage-mode", mode}
	for _, device := range devices {
		args = append(args, "--visible-device", device)
	}
	if deviceMap != "" {
		args = append(args, "--device-map", deviceMap)
	}
	// Driver storage has no helper-owned extent bytes to hash.
	if customStorage && enableChecksumDigest {
		args = append(args, "--checksum")
	}
	helper.mu.Lock()
	files := []*os.File{helper.control, root}
	sessions := make(GPUSessions, len(pids))
	var peers []*os.File
	defer func() {
		for _, peer := range peers {
			_ = peer.Close()
		}
	}()
	for _, pid := range pids {
		client, peer, pairErr := helperSocketPair()
		if pairErr != nil {
			err = pairErr
			break
		}
		helper.nextSession++
		helper.sessionIDs[client] = helper.nextSession
		sessions[strconv.Itoa(pid)] = client
		peers = append(peers, peer)
		args = append(args, "--session", fmt.Sprintf("%d:%d:%d:%d", helper.nextSession, pid, 3+len(files), 4+len(files)))
		files = append(files, client, peer)
	}
	if err == nil {
		_, err = runHelperCLI(ctx, helper.binary, files, args...)
	}
	helper.mu.Unlock()
	// Close the parent's unused peers before drain; otherwise a failed bind can
	// leave the daemon waiting on a socket that never reaches EOF.
	for _, peer := range peers {
		_ = peer.Close()
	}
	peers = nil
	if err != nil {
		return nil, errors.Join(err, helper.Drain(sessions))
	}
	return sessions, nil
}

// RunGPUSessions invokes one batch command. CUDA prepare ordering, overlapping
// transfers and the completion barrier are implemented in the C++ helper.
func RunGPUSessions(ctx context.Context, binary string, sessions GPUSessions, pids []int, save bool, log logr.Logger, observed ...[]int) error {
	if len(pids) == 0 {
		return fmt.Errorf("GPU session participant mismatch")
	}
	targets := pids
	if len(observed) > 0 {
		targets = observed[0]
		if len(targets) != len(pids) {
			return fmt.Errorf("GPU restored PID count mismatch")
		}
	}
	direction := "load"
	if save {
		direction = "save"
	}
	args := []string{"--run-batch", "--direction", direction}
	files := make([]*os.File, 0, len(pids))
	seen := make(map[int]bool, len(pids))
	for i, pid := range pids {
		file := sessions[strconv.Itoa(pid)]
		if file == nil {
			return fmt.Errorf("missing GPU session for PID %d", pid)
		}
		if pid <= 0 || pid > math.MaxInt32 || targets[i] <= 0 || targets[i] > math.MaxInt32 || seen[pid] {
			return fmt.Errorf("invalid GPU PID %d or target PID %d", pid, targets[i])
		}
		seen[pid] = true
		args = append(args, "--session", fmt.Sprintf("%d:%d:%d", pid, 3+len(files), targets[i]))
		files = append(files, file)
	}
	if len(files) != len(sessions) {
		return fmt.Errorf("GPU session participant mismatch")
	}
	// Killing the CLI alone leaves our duplicate endpoints open. Wake the
	// daemon immediately so it cancels and joins its transfers during drain.
	cancelled := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		sessions.halfClose()
		close(cancelled)
	})
	defer func() {
		if !stop() {
			<-cancelled
		}
	}()
	start := time.Now()
	output, err := runHelperCLI(ctx, binary, files, args...)
	if err != nil {
		sessions.halfClose()
	}
	logHelperPhases(output, log)
	log.Info("CUDA helper batch complete", "save", save, "elapsed_seconds", time.Since(start).Seconds())
	return err
}

// Telemetry is diagnostic: malformed output must not turn a completed CUDA
// restore into a failed operation after the workload has already resumed.
func logHelperPhases(output []byte, log logr.Logger) {
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		var phase struct {
			PID       int    `json:"pid"`
			Operation string `json:"operation"`
			Reply     struct {
				Metrics map[string]json.Number `json:"metrics"`
			} `json:"reply"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &phase); err != nil {
			log.Error(err, "Invalid CUDA helper timing output")
			continue
		}
		fields := []any{"pid", phase.PID, "operation", phase.Operation}
		var names []string
		switch phase.Operation {
		case "PREPARE":
			names = []string{"prepare_seconds", "prepare_start_ns", "prepare_end_ns"}
		case "TRANSFER":
			names = []string{"bytes", "transfer_seconds", "transfer_setup_seconds", "storage_request_service_seconds",
				"cuda_wait_seconds", "transfer_start_ns", "transfer_end_ns"}
		case "COMPLETE":
			names = []string{"complete_seconds", "unlock_seconds", "total_seconds"}
		}
		for _, name := range names {
			number := phase.Reply.Metrics[name]
			switch name {
			case "bytes":
				if value, err := strconv.ParseUint(string(number), 10, 64); err == nil {
					fields = append(fields, name, value)
				}
			case "prepare_start_ns", "prepare_end_ns", "transfer_start_ns", "transfer_end_ns":
				if value, err := number.Int64(); err == nil {
					fields = append(fields, name, value)
				}
			default:
				if value, err := number.Float64(); err == nil {
					fields = append(fields, name, value)
				}
			}
		}
		log.Info("CUDA helper phase", fields...)
	}
	if err := scanner.Err(); err != nil {
		log.Error(err, "Invalid CUDA helper timing output")
	}
}

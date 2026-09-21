// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	"github.com/go-logr/logr"
)

// NativeSessions owns transaction-scoped broker connections keyed by captured PID.
type NativeSessions map[string]*os.File

func (sessions NativeSessions) Close() {
	for pid, file := range sessions {
		_ = file.Close()
		delete(sessions, pid)
	}
}

// BindNativeSessions pins each restored PID's future namespace and transaction
// directory. After CRIU, the persistent GPU engine binds the recreated target.
func BindNativeSessions(ctx context.Context, broker pagebroker.Client, transaction string, containerPID int, pids []int, devices []string, deviceMap string, save bool) (NativeSessions, error) {
	direction := pagebroker.BindAllocationSession_LOAD
	if save {
		direction = pagebroker.BindAllocationSession_SAVE
	}
	sessions := make(NativeSessions, len(pids))
	for _, pid := range pids {
		key := strconv.Itoa(pid)
		if pid <= 0 || sessions[key] != nil {
			sessions.Close()
			return nil, fmt.Errorf("invalid or duplicate native PID %d", pid)
		}
		file, err := broker.BindNative(ctx, transaction, &pagebroker.BindNativeSession{
			Direction: direction, ContainerPid: uint32(containerPID), NamespacePid: uint32(pid),
			VisibleDevices: devices, DeviceMap: deviceMap,
		})
		if err != nil {
			sessions.Close()
			return nil, fmt.Errorf("bind native PID %d: %w", pid, err)
		}
		sessions[key] = file
	}
	return sessions, nil
}

// RunNativeSessions keeps CUDA preparation serial. LOAD transfers may overlap
// later preparation; no target is completed/unlocked until every copy succeeds.
func RunNativeSessions(ctx context.Context, sessions NativeSessions, pids []int, save, pipeline bool, log logr.Logger, observed ...[]int) error {
	start := time.Now()
	files := make([]*os.File, len(pids))
	for i, pid := range pids {
		files[i] = sessions[strconv.Itoa(pid)]
		if files[i] == nil {
			return fmt.Errorf("missing native session for PID %d", pid)
		}
	}
	if len(files) != len(sessions) || len(files) == 0 {
		return fmt.Errorf("native session participant mismatch")
	}
	targets := pids
	if len(observed) > 0 {
		targets = observed[0]
		if len(targets) != len(pids) {
			return fmt.Errorf("native restored PID count mismatch")
		}
	}
	run := func(index int, operation pagebroker.NativeSessionRequest_Operation) error {
		begin := time.Now()
		report, err := pagebroker.NativeOperation(ctx, files[index], operation, targets[index])
		log.Info("Native PageBroker phase", "pid", pids[index], "operation", operation.String(),
			"duration", time.Since(begin), "report", report)
		return err
	}
	if save {
		for i := range files {
			if err := run(i, pagebroker.NativeSessionRequest_LOCK); err != nil {
				return err
			}
		}
	}
	var jobs sync.WaitGroup
	errors := make([]error, len(files))
	transfer := func(i int) {
		jobs.Go(func() { errors[i] = run(i, pagebroker.NativeSessionRequest_TRANSFER) })
	}
	for i := range files {
		if err := run(i, pagebroker.NativeSessionRequest_PREPARE); err != nil {
			jobs.Wait()
			return err
		}
		if pipeline && !save {
			transfer(i)
		}
	}
	if !pipeline || save {
		for i := range files {
			transfer(i)
		}
	}
	jobs.Wait()
	for _, err := range errors {
		if err != nil {
			return err
		}
	}
	for i := len(files) - 1; i >= 0; i-- {
		if err := run(i, pagebroker.NativeSessionRequest_COMPLETE); err != nil {
			return err
		}
	}
	log.Info("Native PageBroker batch complete", "save", save, "pipeline", pipeline, "duration", time.Since(start))
	return nil
}

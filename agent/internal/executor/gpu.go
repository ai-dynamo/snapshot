// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/compat"
	"github.com/go-logr/logr"
)

// GPUDrainError means that PageBroker has not confirmed GPU cleanup.
// Keep target processes and files until PageBroker exits or confirms cleanup.
type GPUDrainError struct{ Err error }

func (e *GPUDrainError) Error() string { return e.Err.Error() }
func (e *GPUDrainError) Unwrap() error { return e.Err }

// Use driver-managed capture only when PageBroker reports no CustomStorage
// support. Do not change formats after a connection or execution failure.
func selectGPUCheckpoint(ctx context.Context, config types.PageBrokerSpec, hasCUDA bool) (bool, error) {
	if !hasCUDA || !config.Enabled {
		return false, nil
	}
	capabilities, err := (pagebroker.Client{ControlSocketPath: config.ControlSocketPath}).Capabilities(ctx)
	if err != nil {
		return false, fmt.Errorf("check PageBroker CustomStorage support: %w", err)
	}
	return capabilities.CustomStorageAvailable, nil
}

func gpuUUIDs(info compat.GPUInfo) []string {
	uuids := make([]string, len(info.Devices))
	for i, device := range info.Devices {
		uuids[i] = device.UUID
	}
	return uuids
}

func gpuContext(pids []int, devices []string, deviceMap string) (*pagebroker.GpuContext, error) {
	if len(pids) == 0 || len(devices) == 0 {
		return nil, fmt.Errorf("invalid GPU participant identity")
	}
	context := &pagebroker.GpuContext{VisibleDevices: devices}
	seen := make(map[int]bool, len(pids))
	for _, pid := range pids {
		if pid <= 0 || pid > math.MaxInt32 || seen[pid] {
			return nil, fmt.Errorf("invalid or duplicate captured GPU PID %d", pid)
		}
		seen[pid] = true
		context.CapturedPids = append(context.CapturedPids, uint32(pid))
	}
	if deviceMap != "" {
		for _, pair := range strings.Split(deviceMap, ",") {
			source, target, ok := strings.Cut(pair, "=")
			if !ok || source == "" || target == "" {
				return nil, fmt.Errorf("invalid GPU device mapping")
			}
			context.DeviceMap = append(context.DeviceMap, &pagebroker.GpuDeviceMapping{SourceUuid: source, TargetUuid: target})
		}
	}
	return context, nil
}

func logGPUResult(log logr.Logger, result *pagebroker.GpuComplete) {
	for _, participant := range result.GetParticipants() {
		log.Info("PageBroker GPU participant complete", "captured_pid", participant.CapturedPid,
			"bytes", participant.Bytes)
	}
}

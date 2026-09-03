// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"

	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

const (
	actionLock       = "lock"
	actionCheckpoint = "checkpoint"
	actionRestore    = "restore"
	actionUnlock     = "unlock"
)

var errProcessIdentityChangedBeforeCUDA = errors.New("process identity changed before CUDA driver call")

type helperActionRunner interface {
	run(context.Context, helperAction, logr.Logger) error
}

type helperAction struct {
	PID         int
	Action      string
	DeviceMap   string
	StorageMode string
	StorageDir  string
	JobFile     string
	GPUUUIDs    []string
	Transfer    types.CUDATransferSettings
	Identity    snapshotruntime.ProcessDetails
}

type commandHelperActionRunner struct{}

type identityValidatingRunner struct {
	runner     helperActionRunner
	procRoot   string
	identities map[int]snapshotruntime.ProcessDetails
}

type customStorageTelemetry struct {
	Event                        string          `json:"event"`
	HelperMainToTelemetrySeconds json.RawMessage `json:"helper_main_to_telemetry_seconds"`
}

type customStorageTelemetryParse struct {
	status             string
	err                string
	helperMainDuration time.Duration
}

func parseCustomStorageTelemetry(output string, processWall time.Duration) customStorageTelemetryParse {
	sawMalformedJSON := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var telemetry customStorageTelemetry
		if err := json.Unmarshal([]byte(line), &telemetry); err != nil {
			sawMalformedJSON = true
			continue
		}
		if telemetry.Event != "cuda_custom_storage_transfer" {
			continue
		}
		if len(telemetry.HelperMainToTelemetrySeconds) == 0 || string(telemetry.HelperMainToTelemetrySeconds) == "null" {
			return customStorageTelemetryParse{status: "missing-duration", err: "expected helper_main_to_telemetry_seconds"}
		}
		var seconds json.Number
		if err := json.Unmarshal(telemetry.HelperMainToTelemetrySeconds, &seconds); err != nil {
			return customStorageTelemetryParse{status: "invalid-duration", err: "helper_main_to_telemetry_seconds is not a number"}
		}
		value, err := strconv.ParseFloat(seconds.String(), 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return customStorageTelemetryParse{status: "invalid-duration", err: "helper_main_to_telemetry_seconds is not a finite non-negative number"}
		}
		const roundingToleranceSeconds = 1e-6
		processWallSeconds := processWall.Seconds()
		if value > processWallSeconds+roundingToleranceSeconds {
			return customStorageTelemetryParse{status: "duration-exceeds-process-wall", err: "helper_main_to_telemetry_seconds exceeds process wall duration"}
		}
		if value >= processWallSeconds || value*float64(time.Second) >= float64(math.MaxInt64) {
			return customStorageTelemetryParse{status: "valid", helperMainDuration: processWall}
		}
		return customStorageTelemetryParse{status: "valid", helperMainDuration: time.Duration(value * float64(time.Second))}
	}
	if sawMalformedJSON {
		return customStorageTelemetryParse{status: "malformed-json", err: "malformed JSON telemetry output"}
	}
	return customStorageTelemetryParse{status: "event-absent", err: "cuda_custom_storage_transfer event not found"}
}

func (commandHelperActionRunner) run(
	ctx context.Context,
	request helperAction,
	log logr.Logger,
) error {
	if request.Identity.OutermostPID != request.PID ||
		request.Identity.StartTimeTicks == 0 || request.Identity.Cgroup == "" {
		return fmt.Errorf("incomplete process identity for host PID %d", request.PID)
	}
	if request.Action == actionLock || request.Action == actionUnlock ||
		request.StorageMode == types.CUDAStorageModeLegacy {
		request.StorageDir = ""
	}
	return runDaemonAction(ctx, request, log)
}

func (r identityValidatingRunner) run(
	ctx context.Context,
	request helperAction,
	log logr.Logger,
) error {
	expected, ok := r.identities[request.PID]
	if !ok {
		return fmt.Errorf("%w: missing expected process identity for host PID %d", errProcessIdentityChangedBeforeCUDA, request.PID)
	}
	if err := snapshotruntime.ValidateProcessIdentity(r.procRoot, expected); err != nil {
		return fmt.Errorf("%w: validate host PID %d immediately before CUDA %s: %v", errProcessIdentityChangedBeforeCUDA, request.PID, request.Action, err)
	}
	request.Identity = expected
	return r.runner.run(ctx, request, log)
}

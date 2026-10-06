// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"context"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
)

func (c Client) Capabilities(ctx context.Context) (*Capabilities, error) {
	response, err := c.request(ctx, "", &Request_Capabilities{Capabilities: &CapabilitiesRequest{}})
	if err != nil {
		return nil, err
	}
	if response.GetCapabilities() == nil {
		return nil, fmt.Errorf("unexpected PageBroker capabilities response")
	}
	return response.GetCapabilities(), nil
}

// GPUExecution retains the control socket directory across namespace entry.
// It connects only when the GPU operation starts. Targets are host PIDs.
type GPUExecution struct {
	Directory     *os.File
	SocketName    string
	Context       *GpuContext
	TransactionID string
}

func (g *GPUExecution) Close() error {
	if g == nil {
		return nil
	}
	if g.Directory != nil {
		return g.Directory.Close()
	}
	return nil
}

func (c Client) OpenGPUExecution(transactionID string, gpu *GpuContext) (*GPUExecution, error) {
	if gpu == nil {
		return nil, fmt.Errorf("GPU execution requires context")
	}
	directory, err := os.Open(filepath.Dir(c.ControlSocketPath))
	if err != nil {
		return nil, fmt.Errorf("open PageBroker socket directory: %w", err)
	}
	return &GPUExecution{Directory: directory, SocketName: filepath.Base(c.ControlSocketPath),
		Context: gpu, TransactionID: transactionID}, nil
}

func gpuTargets(captured, hostPIDs []int) ([]*GpuTarget, error) {
	if len(captured) == 0 || len(captured) != len(hostPIDs) {
		return nil, fmt.Errorf("GPU participant count mismatch")
	}
	seenCaptured, seenHost := make(map[int]bool), make(map[int]bool)
	targets := make([]*GpuTarget, len(captured))
	for i, pid := range captured {
		targetPID := hostPIDs[i]
		if pid <= 0 || pid > math.MaxInt32 || targetPID <= 0 || targetPID > math.MaxInt32 || seenCaptured[pid] || seenHost[targetPID] {
			return nil, fmt.Errorf("invalid or duplicate GPU participant")
		}
		seenCaptured[pid], seenHost[targetPID] = true, true
		targets[i] = &GpuTarget{CapturedPid: uint32(pid), TargetPid: uint32(targetPID)}
	}
	return targets, nil
}

func (g *GPUExecution) Checkpoint(ctx context.Context, captured, hostPIDs []int) (*GpuComplete, error) {
	if g == nil {
		return nil, fmt.Errorf("missing PageBroker GPU execution context")
	}
	targets, err := gpuTargets(captured, hostPIDs)
	if err != nil {
		return nil, err
	}
	response, err := g.execute(ctx, &Request_CheckpointGpu{CheckpointGpu: &CheckpointGpuRequest{Targets: targets, Context: g.Context}})
	if err != nil {
		return nil, err
	}
	if response.GetGpuCheckpointComplete() == nil {
		return nil, fmt.Errorf("unexpected PageBroker GPU checkpoint response")
	}
	return response.GetGpuCheckpointComplete(), nil
}

func (g *GPUExecution) Restore(ctx context.Context, captured, hostPIDs []int) (*GpuComplete, error) {
	if g == nil {
		return nil, fmt.Errorf("missing PageBroker GPU execution context")
	}
	targets, err := gpuTargets(captured, hostPIDs)
	if err != nil {
		return nil, err
	}
	response, err := g.execute(ctx, &Request_RestoreGpu{RestoreGpu: &RestoreGpuRequest{Targets: targets, Context: g.Context}})
	if err != nil {
		return nil, err
	}
	if response.GetGpuRestoreComplete() == nil {
		return nil, fmt.Errorf("unexpected PageBroker GPU restore response")
	}
	return response.GetGpuRestoreComplete(), nil
}

func (g *GPUExecution) execute(ctx context.Context, command isRequest_Command) (*Response, error) {
	if g == nil || g.Directory == nil || g.Context == nil || g.TransactionID == "" || g.SocketName == "" {
		return nil, fmt.Errorf("missing PageBroker GPU execution context")
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", fmt.Sprintf("/proc/self/fd/%d/%s", g.Directory.Fd(), g.SocketName))
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return nil, fmt.Errorf("PageBroker GPU execution descriptor is not a Unix socket")
	}
	return exchange(ctx, unixConnection, g.TransactionID, command)
}

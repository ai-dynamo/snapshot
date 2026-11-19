// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

// fabricQuery asks nvidia-smi for the columns parseNvidiaSmiFabric reads.
const fabricQuery = "--query-gpu=uuid,fabric.state,fabric.status,fabric.clusterUuid,fabric.cliqueId"

// ReadGPUFabric reads where each GPU of uuids is in its NVLink fabric, in the
// order of uuids. nvidia-smi runs in the agent, which sees every GPU of the
// node, as it does to resolve an NVIDIA_VISIBLE_DEVICES selection, so the
// container that holds the GPUs needs no nvidia-smi of its own.
func ReadGPUFabric(ctx context.Context, uuids []string) ([]types.GPUFabric, error) {
	ctx, cancel := context.WithTimeout(ctx, nvidiaSMITimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, "nvidia-smi", "-i", strings.Join(uuids, ","),
		fabricQuery, "--format=csv,noheader").Output()
	if err != nil {
		// nvidia-smi says why on stdout, such as that it has no fabric fields.
		if reason := bytes.TrimSpace(output); len(reason) > 0 {
			err = fmt.Errorf("%w: %s", err, reason)
		}
		return nil, fmt.Errorf("nvidia-smi %s: %w", fabricQuery, err)
	}
	listed, err := parseNvidiaSmiFabric(string(output))
	if err != nil {
		return nil, err
	}
	fabrics := make([]types.GPUFabric, len(uuids))
	for i, uuid := range uuids {
		fabric, ok := listed[uuid]
		if !ok {
			return nil, fmt.Errorf("nvidia-smi did not report the NVLink fabric of GPU %s", uuid)
		}
		fabrics[i] = fabric
	}
	return fabrics, nil
}

// parseNvidiaSmiFabric reads fabricQuery's unquoted CSV, a row per GPU, by
// UUID. nvidia-smi prints a value it cannot read as N/A, which leaves the GPU
// not ready. A row without every column is an error, rather than a GPU whose
// fabric is unknown.
func parseNvidiaSmiFabric(output string) (map[string]types.GPUFabric, error) {
	fabrics := map[string]types.GPUFabric{}
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) != 5 {
			return nil, fmt.Errorf("nvidia-smi reported %q, not a GPU's UUID and its NVLink fabric", line)
		}
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		fabrics[fields[0]] = types.GPUFabric{
			UUID: fields[0], State: fields[1], Status: fields[2], ClusterUUID: fields[3], CliqueID: fields[4],
		}
	}
	return fabrics, nil
}

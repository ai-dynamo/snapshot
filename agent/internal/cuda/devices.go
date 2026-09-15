// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/ai-dynamo/snapshot/api/compat"
)

// ResolveVisibleGPUs preserves explicit selection order and includes the model
// and driver metadata used by the restore compatibility gate.
func ResolveVisibleGPUs(ctx context.Context, env []string) (compat.GPUInfo, error) {
	uuids, err := ResolveVisibleDevices(ctx, env)
	if err != nil || len(uuids) == 0 {
		return compat.GPUInfo{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, nvidiaSMITimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, "nvidia-smi", "-i", strings.Join(uuids, ","),
		"--query-gpu=uuid,name,driver_version", "--format=csv,noheader").Output()
	if err != nil {
		return compat.GPUInfo{}, fmt.Errorf("describe NVIDIA_VISIBLE_DEVICES GPUs: %w", err)
	}
	return describeGPUs(uuids, parseNvidiaSmiGPUs(string(output))), nil
}

// ResolveVisibleDevices resolves an explicit legacy runtime selection on the
// host, before container-local CUDA enumeration can reinterpret numeric indices.
// A nil selection leaves allocation discovery to the caller.
func ResolveVisibleDevices(ctx context.Context, env []string) ([]string, error) {
	value := VisibleDevicesValue(env)
	if value == nil {
		return nil, nil
	}
	return resolveVisibleDevices(ctx, *value)
}

// VisibleDevicesValue distinguishes an absent selection from an explicit empty value.
func VisibleDevicesValue(env []string) *string {
	var value *string
	for _, entry := range env {
		if v, ok := strings.CutPrefix(entry, "NVIDIA_VISIBLE_DEVICES="); ok {
			value = &v
		}
	}
	return value
}

func resolveVisibleDevices(ctx context.Context, value string) ([]string, error) {
	switch value {
	case "", "all", "none", "void":
		return nil, nil
	}
	var uuids []string
	seen := map[string]bool{}
	for _, selection := range strings.Split(value, ",") {
		selection = strings.TrimSpace(selection)
		if _, err := strconv.ParseUint(selection, 10, 32); err != nil && !gpuUUIDPattern.MatchString(selection) {
			return nil, fmt.Errorf("unsupported NVIDIA_VISIBLE_DEVICES selection %q", selection)
		}
		output, err := exec.CommandContext(ctx, "nvidia-smi", "-i", selection,
			"--query-gpu=uuid", "--format=csv,noheader").Output()
		if err != nil {
			return nil, fmt.Errorf("resolve NVIDIA_VISIBLE_DEVICES selection %q: %w", selection, err)
		}
		uuid := strings.TrimSpace(string(output))
		if !gpuUUIDPattern.MatchString(uuid) || seen[uuid] {
			return nil, fmt.Errorf("invalid or duplicate resolved GPU UUID %q", uuid)
		}
		seen[uuid] = true
		uuids = append(uuids, uuid)
	}
	return uuids, nil
}

// ResolveDevicePaths associates physical UUIDs with host minor numbers and
// verifies that those exact devices are exposed in the workload namespace.
func ResolveDevicePaths(hostProc string, pid int, uuids []string) (map[string]string, error) {
	inventory := map[string]uint32{}
	files, err := filepath.Glob(filepath.Join(hostProc, "driver/nvidia/gpus/*/information"))
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		var uuid, minor string
		for _, line := range strings.Split(string(data), "\n") {
			key, value, _ := strings.Cut(line, ":")
			switch strings.TrimSpace(key) {
			case "GPU UUID":
				uuid = strings.TrimSpace(value)
			case "Device Minor":
				minor = strings.TrimSpace(value)
			}
		}
		n, err := strconv.ParseUint(minor, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("parse device minor in %s: %w", file, err)
		}
		inventory[uuid] = uint32(n)
	}
	paths := map[string]string{}
	for _, uuid := range uuids {
		minor, ok := inventory[uuid]
		if !ok {
			return nil, fmt.Errorf("no physical device minor for GPU %s", uuid)
		}
		path := fmt.Sprintf("/dev/nvidia%d", minor)
		var stat unix.Stat_t
		if err := unix.Stat(filepath.Join(hostProc, strconv.Itoa(pid), "root", path), &stat); err != nil {
			return nil, fmt.Errorf("GPU %s is not exposed at %s: %w", uuid, path, err)
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFCHR || unix.Major(stat.Rdev) != 195 || unix.Minor(stat.Rdev) != minor {
			return nil, fmt.Errorf("GPU %s device mismatch at %s", uuid, path)
		}
		paths[uuid] = path
	}
	return paths, nil
}

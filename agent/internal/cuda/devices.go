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

// resolveSelectedGPUs resolves host indices (not CUDA ordinals), preserving list
// order. One deadline covers all lookups, including model/driver metadata.
func resolveSelectedGPUs(ctx context.Context, value string) (compat.GPUInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, nvidiaSMITimeout)
	defer cancel()
	var gpus compat.GPUInfo
	seen := map[string]bool{}
	for _, selection := range strings.Split(value, ",") {
		selection = strings.TrimSpace(selection)
		if _, err := strconv.ParseUint(selection, 10, 32); err != nil && !gpuUUIDPattern.MatchString(selection) {
			return gpus, fmt.Errorf("unsupported NVIDIA_VISIBLE_DEVICES selection %q", selection)
		}
		output, err := exec.CommandContext(ctx, "nvidia-smi", "-i", selection,
			"--query-gpu=uuid,name,driver_version", "--format=csv,noheader").Output()
		if err != nil {
			return gpus, fmt.Errorf("resolve NVIDIA_VISIBLE_DEVICES selection %q: %w", selection, err)
		}
		resolved := parseNvidiaSmiGPUs(string(output))
		if len(resolved.Devices) != 1 {
			return gpus, fmt.Errorf("NVIDIA_VISIBLE_DEVICES selection %q did not resolve one GPU", selection)
		}
		uuid := resolved.Devices[0].UUID
		if !gpuUUIDPattern.MatchString(uuid) || seen[uuid] {
			return gpus, fmt.Errorf("invalid or duplicate resolved GPU UUID %q", uuid)
		}
		seen[uuid] = true
		gpus.Devices = append(gpus.Devices, resolved.Devices[0])
		gpus.DriverVersion = resolved.DriverVersion
	}
	return gpus, nil
}

// VisibleDevicesValue distinguishes absent from explicitly empty. Scan backwards
// to match the last-assignment-wins convention used for process environments.
func VisibleDevicesValue(env []string) *string {
	for i := len(env) - 1; i >= 0; i-- {
		if value, ok := strings.CutPrefix(env[i], "NVIDIA_VISIBLE_DEVICES="); ok {
			return &value
		}
	}
	return nil
}

// ResolveDevicePaths associates physical UUIDs with host minor numbers and
// verifies that those exact devices are exposed in the workload namespace.
func ResolveDevicePaths(hostProc string, pid int, uuids []string) (map[string]string, error) {
	if len(uuids) == 0 {
		return nil, nil
	}
	inventory := map[string]uint32{}
	// The NVIDIA kernel driver publishes each physical GPU's UUID and device
	// minor here. The minor names /dev/nvidiaN; it is not a CUDA ordinal.
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
		if err := validateGPUDevice(filepath.Join(hostProc, strconv.Itoa(pid), "root", path), minor); err != nil {
			return nil, fmt.Errorf("GPU %s at %s: %w", uuid, path, err)
		}
		paths[uuid] = path
	}
	return paths, nil
}

// NVIDIA's Linux /dev/nvidiaN character devices use major 195. Checking rdev in
// the workload root prevents a same-named file or another device from qualifying.
// See https://docs.kernel.org/admin-guide/devices.html (195 char).
const nvidiaDeviceMajor = 195

func validateGPUDevice(path string, minor uint32) error {
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFCHR || unix.Major(stat.Rdev) != nvidiaDeviceMajor || unix.Minor(stat.Rdev) != minor {
		return fmt.Errorf("expected NVIDIA character device %d:%d", nvidiaDeviceMajor, minor)
	}
	return nil
}

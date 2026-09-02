// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	"golang.org/x/sys/unix"
)

const customStorageManifestMaximumBytes = 64 * 1024

// DiscoverProcessGPUUUIDs resolves the assigned GPU subset used by each CUDA
// process. CustomStorage retains only those primary contexts so every stream
// returned by the driver belongs to the validated target process GPU set.
func DiscoverProcessGPUUUIDs(
	ctx context.Context,
	pids []int,
	allowedUUIDs []string,
	log logr.Logger,
) (map[int][]string, error) {
	output, err := exec.CommandContext(
		ctx,
		"nvidia-smi",
		"--query-compute-apps=pid,gpu_uuid",
		"--format=csv,noheader,nounits",
	).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("query per-process GPU UUIDs: %w (output: %s)", err, output)
	}
	result, err := parseProcessGPUUUIDs(string(output), pids, allowedUUIDs)
	if err != nil {
		return nil, err
	}
	log.V(1).Info("Resolved per-process GPU UUIDs for CustomStorage", "process_gpu_uuids", result)
	return result, nil
}

func parseProcessGPUUUIDs(output string, pids []int, allowedUUIDs []string) (map[int][]string, error) {
	requested := make(map[int]struct{}, len(pids))
	for _, pid := range pids {
		if pid <= 0 {
			return nil, fmt.Errorf("invalid CUDA PID %d", pid)
		}
		requested[pid] = struct{}{}
	}
	allowed := canonicalUUIDSet(allowedUUIDs)
	if len(allowedUUIDs) == 0 || len(allowed) != len(allowedUUIDs) {
		return nil, errors.New("assigned GPU UUID list contains an invalid or duplicate UUID")
	}

	result := make(map[int][]string, len(pids))
	seen := make(map[int]map[string]struct{}, len(pids))
	for lineNumber, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid nvidia-smi compute-process row %d", lineNumber+1)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil || pid <= 0 {
			return nil, fmt.Errorf("invalid nvidia-smi PID on row %d", lineNumber+1)
		}
		if _, wanted := requested[pid]; !wanted {
			continue
		}
		uuidKey := strings.ToLower(strings.TrimSpace(fields[1]))
		uuid, assigned := allowed[uuidKey]
		if !assigned {
			return nil, fmt.Errorf("CUDA PID %d uses GPU %q outside the container allocation", pid, strings.TrimSpace(fields[1]))
		}
		if seen[pid] == nil {
			seen[pid] = make(map[string]struct{})
		}
		if _, duplicate := seen[pid][uuidKey]; duplicate {
			continue
		}
		seen[pid][uuidKey] = struct{}{}
		result[pid] = append(result[pid], uuid)
	}
	for _, pid := range pids {
		if len(result[pid]) == 0 {
			// Driver-confirmed VMM allocation owners need not appear in NVML's
			// compute-process table. Keep those processes within the validated pod
			// allocation; the helper validates the actual driver-returned streams.
			result[pid] = append([]string(nil), allowedUUIDs...)
		}
	}
	return result, nil
}

func customStorageTargetGPUUUIDs(processDir, deviceMap string, allowedUUIDs []string) ([]string, error) {
	sourceUUIDs, err := readCustomStorageSourceUUIDs(filepath.Join(processDir, "manifest.txt"))
	if err != nil {
		return nil, err
	}
	mapping, err := parseDeviceMapUUIDs(deviceMap)
	if err != nil {
		return nil, err
	}
	allowed := canonicalUUIDSet(allowedUUIDs)
	if len(allowed) != len(allowedUUIDs) {
		return nil, errors.New("target GPU UUID list contains an invalid or duplicate UUID")
	}
	if len(sourceUUIDs) == 0 {
		return append([]string(nil), allowedUUIDs...), nil
	}
	result := make([]string, 0, len(sourceUUIDs))
	seen := make(map[string]struct{}, len(sourceUUIDs))
	for _, source := range sourceUUIDs {
		target := source
		if len(mapping) > 0 {
			var ok bool
			target, ok = mapping[strings.ToLower(source)]
			if !ok {
				return nil, fmt.Errorf("source GPU %s is missing from the restore device map", source)
			}
		}
		targetKey := strings.ToLower(target)
		canonical, ok := allowed[targetKey]
		if !ok {
			return nil, fmt.Errorf("CustomStorage target GPU %s is outside the restore allocation", target)
		}
		if _, duplicate := seen[targetKey]; duplicate {
			return nil, fmt.Errorf("CustomStorage manifest maps multiple source GPUs to %s", canonical)
		}
		seen[targetKey] = struct{}{}
		result = append(result, canonical)
	}
	return result, nil
}

func readCustomStorageSourceUUIDs(path string) (uuids []string, err error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open CUDA CustomStorage manifest: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() {
		if closeErr := file.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close CUDA CustomStorage manifest: %w", closeErr)
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > customStorageManifestMaximumBytes {
		return nil, errors.New("CUDA CustomStorage manifest is not a bounded nonempty regular file")
	}
	reader := bufio.NewReader(io.LimitReader(file, customStorageManifestMaximumBytes+1))
	var key string
	var version, count int
	if _, err := fmt.Fscan(reader, &key, &version); err != nil || key != "version" || version != 2 {
		return nil, errors.New("invalid CUDA CustomStorage manifest version")
	}
	if _, err := fmt.Fscan(reader, &key, &count); err != nil || key != "device_count" || count < 0 || count > 1024 {
		return nil, errors.New("invalid CUDA CustomStorage manifest device count")
	}
	seen := make(map[string]struct{}, count)
	for expected := 0; expected < count; expected++ {
		var index int
		var uuid, filename string
		var size uint64
		if _, err := fmt.Fscan(reader, &key, &index, &uuid, &size, &filename); err != nil ||
			key != "device" || index != expected || !gpuUUIDPattern.MatchString(uuid) ||
			size == 0 || filename != fmt.Sprintf("device-%04d.bin", expected) {
			return nil, fmt.Errorf("invalid CUDA CustomStorage manifest device %d", expected)
		}
		uuidKey := strings.ToLower(uuid)
		if _, duplicate := seen[uuidKey]; duplicate {
			return nil, fmt.Errorf("duplicate GPU UUID %s in CUDA CustomStorage manifest", uuid)
		}
		seen[uuidKey] = struct{}{}
		uuids = append(uuids, uuid)
	}
	var extra string
	if _, err := fmt.Fscan(reader, &extra); err != io.EOF {
		return nil, errors.New("CUDA CustomStorage manifest has trailing data")
	}
	return uuids, nil
}

func parseDeviceMapUUIDs(value string) (map[string]string, error) {
	if value == "" {
		return nil, nil
	}
	result := make(map[string]string)
	for _, pair := range strings.Split(value, ",") {
		parts := strings.Split(pair, "=")
		if len(parts) != 2 || !gpuUUIDPattern.MatchString(parts[0]) || !gpuUUIDPattern.MatchString(parts[1]) {
			return nil, fmt.Errorf("invalid CUDA device-map pair %q", pair)
		}
		key := strings.ToLower(parts[0])
		if _, duplicate := result[key]; duplicate {
			return nil, fmt.Errorf("duplicate source GPU %s in CUDA device map", parts[0])
		}
		result[key] = parts[1]
	}
	return result, nil
}

func canonicalUUIDSet(uuids []string) map[string]string {
	result := make(map[string]string, len(uuids))
	for _, uuid := range uuids {
		if !gpuUUIDPattern.MatchString(uuid) {
			return nil
		}
		key := strings.ToLower(uuid)
		if _, duplicate := result[key]; duplicate {
			return nil
		}
		result[key] = uuid
	}
	return result
}

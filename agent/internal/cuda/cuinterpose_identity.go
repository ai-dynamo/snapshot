// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// CuInterposeBundlePath is the exact bundle supplied by ns-bind-mount at restore.
const CuInterposeBundlePath = "/snapshot-binaries/snapshot-cuda"

// InspectCuInterposeLibraries uses the existing CUDA process census, not the
// set of responding sockets. Missing participants must not disappear from inspection.
func InspectCuInterposeLibraries(procRoot string, pids []int, required bool) (*types.CuInterposeManifest, error) {
	var identity *types.CuInterposeManifest
	var absent []int
	for _, pid := range pids {
		current, err := processCuInterposeLibraries(filepath.Join(procRoot, strconv.Itoa(pid)))
		if err != nil {
			return nil, fmt.Errorf("cuinterpose process %d: %w", pid, err)
		}
		if current == nil {
			absent = append(absent, pid)
			continue
		}
		if identity != nil && *identity != *current {
			return nil, fmt.Errorf("cuinterpose process %d: library hashes differ between CUDA participants", pid)
		}
		identity = current
	}
	if identity != nil && len(absent) != 0 {
		return nil, fmt.Errorf("cuinterpose is missing from CUDA participants %v", absent)
	}
	if identity == nil && required {
		return nil, fmt.Errorf("cuinterpose was requested or delivered but is not active in any CUDA participant")
	}
	return identity, nil
}

func processCuInterposeLibraries(processDir string) (*types.CuInterposeManifest, error) {
	maps, err := os.Open(filepath.Join(processDir, "maps"))
	if err != nil {
		return nil, err
	}
	defer maps.Close()
	var identity types.CuInterposeManifest
	scanner := bufio.NewScanner(maps)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 6 {
			continue
		}
		path := strings.TrimSuffix(strings.Join(fields[5:], " "), " (deleted)")
		var digest *string
		switch filepath.Base(path) {
		case "libcuinterpose.so":
			digest = &identity.FrontendSHA256
		case "libcuinterpose_core.so":
			digest = &identity.CoreSHA256
		default:
			continue
		}
		if path != filepath.Join(podcontract.CuInterposeMountPath, filepath.Base(path)) {
			return nil, fmt.Errorf("library %s must be delivered at %s before startup", path, podcontract.CuInterposeMountPath)
		}
		if len(fields) > 6 {
			return nil, fmt.Errorf("mapped library %s is deleted or has an unsupported path", path)
		}
		hash, err := hashMappedLibrary(filepath.Join(processDir, "root", path), fields[3], fields[4])
		if err != nil {
			return nil, err
		}
		if *digest != "" && *digest != hash {
			return nil, fmt.Errorf("mapped library %s changed during inspection", path)
		}
		*digest = hash
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if identity.FrontendSHA256 == "" && identity.CoreSHA256 == "" {
		return nil, nil
	}
	if identity.FrontendSHA256 == "" || identity.CoreSHA256 == "" {
		return nil, fmt.Errorf("both cuinterpose frontend and core must be mapped")
	}
	return &identity, nil
}

func hashMappedLibrary(path, device, inode string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open mapped library %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	stat := info.Sys().(*syscall.Stat_t)
	wantDevice := fmt.Sprintf("%02x:%02x", unix.Major(stat.Dev), unix.Minor(stat.Dev))
	if device != wantDevice || inode != strconv.FormatUint(stat.Ino, 10) {
		return "", fmt.Errorf("mapped library %s was replaced since it was loaded", path)
	}
	// Hash the same descriptor checked against /proc/maps. Delivery mounts are
	// read-only; manually delivered libraries must also remain stable during capture.
	return hashLibrary(file)
}

func hashLibrary(file *os.File) (string, error) {
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash library %s: %w", file.Name(), err)
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

// CheckCuInterposeLibraries is an executable identity check, independent of
// compatibility policy and its debugging override.
func CheckCuInterposeLibraries(directory string, identity *types.CuInterposeManifest) error {
	if identity == nil {
		return nil
	}
	if err := identity.Validate(); err != nil {
		return err
	}
	for _, library := range []struct{ name, expected string }{
		{"libcuinterpose.so", identity.FrontendSHA256},
		{"libcuinterpose_core.so", identity.CoreSHA256},
	} {
		file, err := os.Open(filepath.Join(directory, library.name))
		if err != nil {
			return fmt.Errorf("open restore library %s: %w", library.name, err)
		}
		actual, err := hashLibrary(file)
		file.Close()
		if err != nil {
			return err
		}
		if !strings.EqualFold(library.expected, actual) {
			return fmt.Errorf("cuinterpose %s SHA-256 mismatch: expected %s, actual %s; use matching shim libraries or recreate the checkpoint", library.name, library.expected, actual)
		}
	}
	return nil
}

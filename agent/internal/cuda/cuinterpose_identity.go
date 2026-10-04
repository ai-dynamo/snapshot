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

type CuInterposeRequirement int

const (
	CuInterposeOptional CuInterposeRequirement = iota
	CuInterposeRequired
)

// InspectCuInterposeLibraries uses the CUDA process list so a missing socket reply
// cannot silently remove a participant from inspection.
func InspectCuInterposeLibraries(procRoot string, pids []int, requirement CuInterposeRequirement) (*types.CuInterposeManifest, error) {
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
	if identity == nil && requirement == CuInterposeRequired {
		return nil, fmt.Errorf("cuinterpose was requested or delivered but is not active in any CUDA participant")
	}
	return identity, nil
}

type mappedLibrary struct {
	device string
	inode  string
}

func processCuInterposeLibraries(processDir string) (*types.CuInterposeManifest, error) {
	maps, err := os.Open(filepath.Join(processDir, "maps"))
	if err != nil {
		return nil, err
	}
	defer maps.Close()
	libraries := make(map[string]mappedLibrary)
	scanner := bufio.NewScanner(maps)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 6 {
			continue
		}
		path := strings.TrimSuffix(strings.Join(fields[5:], " "), " (deleted)")
		switch filepath.Base(path) {
		case "libcuinterpose.so", "libcuinterpose_core.so":
		default:
			continue
		}
		if path != filepath.Join(podcontract.CuInterposeMountPath, filepath.Base(path)) {
			return nil, fmt.Errorf("library %s must be delivered at %s before startup", path, podcontract.CuInterposeMountPath)
		}
		if len(fields) > 6 {
			return nil, fmt.Errorf("mapped library %s is deleted or has an unsupported path", path)
		}
		mapping := mappedLibrary{device: fields[3], inode: fields[4]}
		if previous, found := libraries[path]; found && previous != mapping {
			return nil, fmt.Errorf("mapped library %s has conflicting device/inode identities", path)
		}
		libraries[path] = mapping
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(libraries) == 0 {
		return nil, nil
	}
	if len(libraries) != 2 {
		return nil, fmt.Errorf("both cuinterpose frontend and core must be mapped")
	}
	var identity types.CuInterposeManifest
	for _, library := range []struct {
		name   string
		digest *string
	}{
		{"libcuinterpose.so", &identity.FrontendSHA256},
		{"libcuinterpose_core.so", &identity.CoreSHA256},
	} {
		path := filepath.Join(podcontract.CuInterposeMountPath, library.name)
		hash, err := hashMappedLibrary(filepath.Join(processDir, "root", path), libraries[path])
		if err != nil {
			return nil, err
		}
		*library.digest = hash
	}
	return &identity, nil
}

func hashMappedLibrary(path string, mapping mappedLibrary) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open mapped library %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("stat mapped library %s: %w", path, err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	wantDevice := fmt.Sprintf("%02x:%02x", unix.Major(stat.Dev), unix.Minor(stat.Dev))
	if mapping.device != wantDevice || mapping.inode != strconv.FormatUint(stat.Ino, 10) {
		return "", fmt.Errorf("mapped library %s was replaced since it was loaded", path)
	}
	// All mapped regions agree on this descriptor's identity, so hash it once. Delivery
	// mounts are read-only, and manually delivered libraries must also remain unchanged
	// during capture. This does not detect concurrent in-place writes to the same inode.
	return hashLibrary(file)
}

func hashLibrary(file *os.File) (string, error) {
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash library %s: %w", file.Name(), err)
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

// CheckCuInterposeLibraries verifies executable identity. Compatibility policy and its
// debugging override do not affect this check.
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

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

// InspectCuInterposeLibraries verifies the bundle in every CUDA process and returns
// the host PIDs with a mapped core. Socket availability never removes a participant
// from this set. The caller records the corresponding namespace PIDs in the manifest.
func InspectCuInterposeLibraries(procRoot string, pids []int, requirement CuInterposeRequirement) (*types.CuInterposeManifest, []int, error) {
	var identity *types.CuInterposeManifest
	var absent, active []int
	for _, pid := range pids {
		current, coreMapped, err := processCuInterposeLibraries(filepath.Join(procRoot, strconv.Itoa(pid)))
		if err != nil {
			return nil, nil, fmt.Errorf("cuinterpose process %d: %w", pid, err)
		}
		if current == nil {
			absent = append(absent, pid)
			continue
		}
		if identity != nil && (identity.FrontendSHA256 != current.FrontendSHA256 || identity.CoreSHA256 != current.CoreSHA256) {
			return nil, nil, fmt.Errorf("cuinterpose process %d: library hashes differ between CUDA participants", pid)
		}
		identity = current
		if coreMapped {
			active = append(active, pid)
		}
	}
	if identity != nil && len(absent) != 0 {
		return nil, nil, fmt.Errorf("cuinterpose is missing from CUDA participants %v", absent)
	}
	if identity == nil && requirement == CuInterposeRequired {
		return nil, nil, fmt.Errorf("cuinterpose was requested or delivered but is not active in any CUDA participant")
	}
	return identity, active, nil
}

type mappedLibrary struct {
	device string
	inode  string
}

func processCuInterposeLibraries(processDir string) (*types.CuInterposeManifest, bool, error) {
	maps, err := os.Open(filepath.Join(processDir, "maps"))
	if err != nil {
		return nil, false, err
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
			return nil, false, fmt.Errorf("library %s must be delivered at %s before startup", path, podcontract.CuInterposeMountPath)
		}
		if len(fields) > 6 {
			return nil, false, fmt.Errorf("mapped library %s is deleted or has an unsupported path", path)
		}
		mapping := mappedLibrary{device: fields[3], inode: fields[4]}
		if previous, found := libraries[path]; found && previous != mapping {
			return nil, false, fmt.Errorf("mapped library %s has conflicting device/inode identities", path)
		}
		libraries[path] = mapping
	}
	if err := scanner.Err(); err != nil {
		return nil, false, err
	}
	if len(libraries) == 0 {
		return nil, false, nil
	}
	if _, found := libraries[filepath.Join(podcontract.CuInterposeMountPath, "libcuinterpose.so")]; !found {
		return nil, false, fmt.Errorf("cuinterpose frontend must be mapped")
	}
	_, coreMapped := libraries[filepath.Join(podcontract.CuInterposeMountPath, "libcuinterpose_core.so")]
	identity := types.CuInterposeManifest{PIDs: []int{}}
	for _, library := range []struct {
		name   string
		digest *string
	}{
		{"libcuinterpose.so", &identity.FrontendSHA256},
		{"libcuinterpose_core.so", &identity.CoreSHA256},
	} {
		path := filepath.Join(podcontract.CuInterposeMountPath, library.name)
		var mapping *mappedLibrary
		if mapped, found := libraries[path]; found {
			mapping = &mapped
		}
		hash, err := hashDeliveredLibrary(filepath.Join(processDir, "root", path), mapping)
		if err != nil {
			return nil, false, err
		}
		*library.digest = hash
	}
	return &identity, coreMapped, nil
}

func hashDeliveredLibrary(path string, mapping *mappedLibrary) (string, error) {
	// O_NONBLOCK prevents a malformed bundle containing a FIFO from blocking inspection.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("open delivered library %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("stat delivered library %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("library %s must be a regular file", path)
	}
	stat := info.Sys().(*syscall.Stat_t)
	wantDevice := fmt.Sprintf("%02x:%02x", unix.Major(stat.Dev), unix.Minor(stat.Dev))
	if mapping != nil && (mapping.device != wantDevice || mapping.inode != strconv.FormatUint(stat.Ino, 10)) {
		return "", fmt.Errorf("mapped library %s was replaced since it was loaded", path)
	}
	// A frontend-only process has not loaded the lazy core, but must still carry
	// the same bundle so it can initialize after restore. Hash each file once. Delivery
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

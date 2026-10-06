// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// CuInterposeInspection is what capture found in the CUDA processes. It becomes the
// manifest once host PIDs are translated to namespace PIDs.
type CuInterposeInspection struct {
	// sha256 maps each shim library file name to its hash.
	sha256 map[string]string
	// coordinatorHostPIDs are the host PIDs with a mapped core.
	coordinatorHostPIDs []int
}

// Manifest translates coordinator host PIDs through the parallel CUDA PID lists.
func (i *CuInterposeInspection) Manifest(cudaHostPIDs, cudaNamespacePIDs []int) *types.CuInterposeManifest {
	manifest := &types.CuInterposeManifest{SHA256: i.sha256, PIDs: []int{}}
	for index, pid := range cudaHostPIDs {
		if slices.Contains(i.coordinatorHostPIDs, pid) {
			manifest.PIDs = append(manifest.PIDs, cudaNamespacePIDs[index])
		}
	}
	return manifest
}

// InspectCuInterposeLibraries verifies that every CUDA process loaded the same shim
// bundle. It returns nil when no process loaded the shim, which is an error for a
// workload that requested it.
func InspectCuInterposeLibraries(procRoot string, hostPIDs []int, requested bool) (*CuInterposeInspection, error) {
	var inspection *CuInterposeInspection
	var missing []int
	for _, pid := range hostPIDs {
		shim, err := inspectProcessShim(filepath.Join(procRoot, strconv.Itoa(pid)))
		if err != nil {
			return nil, fmt.Errorf("cuinterpose process %d: %w", pid, err)
		}
		if shim == nil {
			missing = append(missing, pid)
			continue
		}
		if inspection == nil {
			inspection = &CuInterposeInspection{sha256: shim.sha256}
		} else if !maps.Equal(inspection.sha256, shim.sha256) {
			return nil, fmt.Errorf("cuinterpose process %d: library hashes differ between CUDA participants", pid)
		}
		if shim.coreMapped {
			inspection.coordinatorHostPIDs = append(inspection.coordinatorHostPIDs, pid)
		}
	}
	if inspection != nil && len(missing) != 0 {
		return nil, fmt.Errorf("cuinterpose is missing from CUDA participants %v", missing)
	}
	if inspection == nil && requested {
		return nil, fmt.Errorf("cuinterpose was requested but is not active in any CUDA participant")
	}
	return inspection, nil
}

type processShim struct {
	sha256     map[string]string
	coreMapped bool
}

// inspectProcessShim returns nil when the process mapped no shim library.
func inspectProcessShim(processDir string) (*processShim, error) {
	mapped, err := mappedShimLibraries(processDir)
	if err != nil || len(mapped) == 0 {
		return nil, err
	}
	if _, found := mapped[types.CuInterposeFrontend]; !found {
		return nil, fmt.Errorf("cuinterpose frontend must be mapped")
	}
	_, coreMapped := mapped[types.CuInterposeCore]
	shim := &processShim{sha256: make(map[string]string, len(types.CuInterposeLibraries)), coreMapped: coreMapped}
	// A frontend-only process has not loaded the lazy core, but must still carry
	// the same bundle so it can initialize after restore.
	for _, library := range types.CuInterposeLibraries {
		var mapping *mappedLibrary
		if mappedLibrary, found := mapped[library]; found {
			mapping = &mappedLibrary
		}
		if shim.sha256[library], err = hashDeliveredLibrary(processDir, library, mapping); err != nil {
			return nil, err
		}
	}
	return shim, nil
}

type mappedLibrary struct {
	device string
	inode  string
}

// mappedShimLibraries returns the shim libraries in a process's maps, keyed by file
// name. Each must be mapped from the delivery path with one device and inode.
func mappedShimLibraries(processDir string) (map[string]mappedLibrary, error) {
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
		name := filepath.Base(path)
		if !slices.Contains(types.CuInterposeLibraries, name) {
			continue
		}
		if path != filepath.Join(podcontract.CuInterposeMountPath, name) {
			return nil, fmt.Errorf("library %s must be delivered at %s before startup", path, podcontract.CuInterposeMountPath)
		}
		if len(fields) > 6 {
			return nil, fmt.Errorf("mapped library %s is deleted or has an unsupported path", path)
		}
		mapping := mappedLibrary{device: fields[3], inode: fields[4]}
		if previous, found := libraries[name]; found && previous != mapping {
			return nil, fmt.Errorf("mapped library %s has conflicting device/inode identities", path)
		}
		libraries[name] = mapping
	}
	return libraries, scanner.Err()
}

// hashDeliveredLibrary hashes a library through the process root. A mapped library
// must still be the file that was loaded.
func hashDeliveredLibrary(processDir, name string, mapping *mappedLibrary) (string, error) {
	path := filepath.Join(processDir, "root", podcontract.CuInterposeMountPath, name)
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
	// Delivery mounts are read-only, and manually delivered libraries must also remain
	// unchanged during capture. This does not detect concurrent in-place writes to the
	// same inode.
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
	for _, library := range types.CuInterposeLibraries {
		file, err := os.Open(filepath.Join(directory, library))
		if err != nil {
			return fmt.Errorf("open restore library %s: %w", library, err)
		}
		actual, err := hashLibrary(file)
		file.Close()
		if err != nil {
			return err
		}
		if expected := identity.SHA256[library]; !strings.EqualFold(expected, actual) {
			return fmt.Errorf("cuinterpose %s SHA-256 mismatch: expected %s, actual %s; use matching shim libraries or recreate the checkpoint", library, expected, actual)
		}
	}
	return nil
}

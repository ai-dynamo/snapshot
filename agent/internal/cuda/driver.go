// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	// DriverLibraryEnv identifies the pinned trusted driver in nsrestore and helpers.
	DriverLibraryEnv    = "SNAPSHOT_CUDA_DRIVER_LIBRARY"
	DriverCompanionsEnv = "SNAPSHOT_CUDA_DRIVER_COMPANIONS"
	HostDriverLibrary   = "/usr/lib/x86_64-linux-gnu/libcuda.so.1"
)

// helperCommand pins a content-matching trusted driver until the caller closes it.
// A nil driver means that the process has no mapped libcuda.
func helperCommand(ctx context.Context, pid int, helper string, args ...string) (*exec.Cmd, *os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	hostLibrary := os.Getenv(DriverLibraryEnv)
	if hostLibrary == "" {
		hostLibrary = HostDriverLibrary
	}
	hostLibrary = strings.Replace(hostLibrary, "/proc/self/", fmt.Sprintf("/proc/%d/", os.Getpid()), 1)
	driver, companions, err := helperDriverLibrary(fmt.Sprintf("/proc/%d", pid), hostLibrary, filepath.Join(filepath.Dir(helper), "cuda-compat"))
	if err != nil {
		return nil, nil, fmt.Errorf("select CUDA library for pid %d: %w", pid, err)
	}
	cmd := exec.CommandContext(ctx, helper, args...)
	if driver != nil {
		if driver.Name() == hostLibrary && os.Getenv(DriverCompanionsEnv) != "" {
			companions = strings.Replace(os.Getenv(DriverCompanionsEnv), "/proc/self/", fmt.Sprintf("/proc/%d/", os.Getpid()), 1)
		}
		cmd.ExtraFiles = []*os.File{driver}
		cmd.Env = append(os.Environ(), "LD_PRELOAD=/proc/self/fd/3", DriverLibraryEnv+"=/proc/self/fd/3")
		libraryPath := companions
		if inherited := os.Getenv("LD_LIBRARY_PATH"); inherited != "" {
			libraryPath += ":" + inherited
		}
		cmd.Env = append(cmd.Env, "LD_LIBRARY_PATH="+libraryPath)
	}
	return cmd, driver, nil
}

func helperDriverLibrary(procDir, hostLibrary, compatDir string) (*os.File, string, error) {
	maps, err := os.ReadFile(filepath.Join(procDir, "maps"))
	if err != nil {
		return nil, "", err
	}
	var mapping, identity string
	for line := range strings.SplitSeq(string(maps), "\n") {
		name := filepath.Base(strings.TrimSuffix(line, " (deleted)"))
		if name != "libcuda.so" && !strings.HasPrefix(name, "libcuda.so.") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 6 {
			return nil, "", fmt.Errorf("invalid CUDA mapping")
		}
		if identity != "" && identity != fields[3]+":"+fields[4] {
			return nil, "", fmt.Errorf("multiple CUDA libraries mapped")
		}
		identity = fields[3] + ":" + fields[4]
		mapping = fields[0]
	}
	if mapping == "" {
		return nil, "", nil
	}
	// map_files opens the mapped inode, not a workload-controlled replacement at
	// its former pathname. Read it only as data; never execute a workload library.
	target, err := os.Open(filepath.Join(procDir, "map_files", mapping))
	if err != nil {
		return nil, "", fmt.Errorf("open mapped CUDA library: %w", err)
	}
	defer target.Close()
	targetInfo, err := target.Stat()
	if err != nil {
		return nil, "", err
	}
	var major, minor, inode uint64
	if _, err := fmt.Sscanf(identity, "%x:%x:%d", &major, &minor, &inode); err != nil {
		return nil, "", fmt.Errorf("invalid CUDA mapping identity: %w", err)
	}
	stat := targetInfo.Sys().(*syscall.Stat_t)
	if !targetInfo.Mode().IsRegular() || uint64(unix.Major(stat.Dev)) != major || uint64(unix.Minor(stat.Dev)) != minor || stat.Ino != inode {
		return nil, "", fmt.Errorf("CUDA mapping changed during inspection")
	}
	var targetDigest []byte
	candidates, err := filepath.Glob(filepath.Join(compatDir, "*", "libcuda.so.1"))
	if err != nil {
		return nil, "", err
	}
	candidates = append([]string{hostLibrary}, candidates...)
	for _, path := range candidates {
		candidate, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, "", err
		}
		info, err := candidate.Stat()
		if err != nil {
			candidate.Close()
			return nil, "", err
		}
		if !info.Mode().IsRegular() || info.Size() != targetInfo.Size() {
			candidate.Close()
			continue
		}
		matches := os.SameFile(info, targetInfo)
		if !matches && targetDigest == nil {
			digest := sha256.New()
			if _, err := io.CopyN(digest, target, targetInfo.Size()); err != nil {
				candidate.Close()
				return nil, "", err
			}
			targetDigest = digest.Sum(nil)
		}
		if !matches {
			digest := sha256.New()
			if _, err := io.CopyN(digest, candidate, info.Size()); err != nil {
				candidate.Close()
				return nil, "", err
			}
			matches = string(targetDigest) == string(digest.Sum(nil))
		}
		if matches {
			return candidate, filepath.Dir(path), nil
		}
		candidate.Close()
	}
	return nil, "", fmt.Errorf("no agent-owned library matches the mapped CUDA library (pid %s)", filepath.Base(procDir))
}

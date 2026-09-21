// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"golang.org/x/sys/unix"
)

// GetNetNSInode returns the network namespace inode for a container process via /host/proc.
func GetNetNSInode(pid int) (uint64, error) {
	nsPath := fmt.Sprintf("%s/%d/ns/net", HostProcPath, pid)
	var stat unix.Stat_t
	if err := unix.Stat(nsPath, &stat); err != nil {
		return 0, fmt.Errorf("failed to stat %s: %w", nsPath, err)
	}
	return stat.Ino, nil
}

// The caller owns mountNS. closeFiles releases the binary and root opened here.
func CommandInNamespaces(ctx context.Context, pid int, mountNS *os.File, rootPath, binaryPath string) (*exec.Cmd, func(), error) {
	if mountNS == nil {
		return nil, nil, fmt.Errorf("mount namespace fd is required")
	}
	binary, err := os.Open(binaryPath)
	if err != nil {
		return nil, nil, err
	}
	root, err := os.Open(rootPath)
	if err != nil {
		binary.Close()
		return nil, nil, err
	}
	closeFiles := func() {
		binary.Close()
		root.Close()
	}
	cmd := exec.CommandContext(ctx, "nsenter")
	mountPath := InheritFile(cmd, mountNS)
	binaryPath = InheritFile(cmd, binary)
	rootPath = InheritFile(cmd, root)
	cmd.Args = append(cmd.Args,
		"--mount="+mountPath, "-t", strconv.Itoa(pid), "-u", "-i", "-n", "-p",
		// CRIU needs the host cgroup namespace. Enter the container root as well
		// as its mount namespace so workload paths resolve inside the container.
		"--root="+rootPath, "--wd="+rootPath, "--", binaryPath,
	)
	return cmd, closeFiles, nil
}

// ExtraFiles[i] becomes fd 3+i in the child. The caller retains file ownership.
func InheritFile(cmd *exec.Cmd, file *os.File) string {
	path := fmt.Sprintf("/proc/self/fd/%d", 3+len(cmd.ExtraFiles))
	cmd.ExtraFiles = append(cmd.ExtraFiles, file)
	return path
}

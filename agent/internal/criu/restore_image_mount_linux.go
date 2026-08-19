// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package criu

import (
	"errors"
	"fmt"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type mountFilesImageOps struct {
	openTree     func(int, string, uint) (int, error)
	mountSetattr func(int, string, uint, *unix.MountAttr) error
	moveMount    func(int, string, int, string, int) error
	unmount      func(string, int) error
	closeFD      func(int) error
}

var realMountFilesImageOps = mountFilesImageOps{
	openTree:     unix.OpenTree,
	mountSetattr: unix.MountSetattr,
	moveMount:    unix.MoveMount,
	unmount:      unix.Unmount,
	closeFD:      unix.Close,
}

func mountFilesImage(checkpointPath, replacementFilesImagePath string) (func() error, error) {
	// checkpointPath is the artifact mount in one restore's private placeholder
	// mount namespace. Mounting two replacements over the same path in one
	// namespace is unsupported because the mounts would stack and cleanup order
	// could expose the wrong restore's metadata.
	return mountFilesImageWithOps(checkpointPath, replacementFilesImagePath, realMountFilesImageOps)
}

func mountFilesImageWithOps(
	checkpointPath string,
	replacementFilesImagePath string,
	ops mountFilesImageOps,
) (func() error, error) {
	// Clone only the rewritten metadata file into a detached mount. The source
	// path is resolved now; after this call the mount pins the file's inode and
	// does not depend on the scratch path remaining reachable during restore.
	mountFD, err := ops.openTree(
		unix.AT_FDCWD,
		replacementFilesImagePath,
		unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC,
	)
	if err != nil {
		return nil, fmt.Errorf("create detached bind mount for rewritten %s: %w", filesImageFilename, err)
	}
	defer func() { _ = ops.closeFD(mountFD) }()

	// Apply immutable data-mount attributes before the mount becomes visible in
	// the restore namespace.
	attr := &unix.MountAttr{
		Attr_set: unix.MOUNT_ATTR_RDONLY | unix.MOUNT_ATTR_NOSUID | unix.MOUNT_ATTR_NODEV | unix.MOUNT_ATTR_NOEXEC,
	}
	if err := ops.mountSetattr(mountFD, "", unix.AT_EMPTY_PATH, attr); err != nil {
		return nil, fmt.Errorf("make rewritten %s mount read-only: %w", filesImageFilename, err)
	}

	targetPath := filepath.Join(checkpointPath, filesImageFilename)
	if err := ops.moveMount(
		mountFD,
		"",
		unix.AT_FDCWD,
		targetPath,
		unix.MOVE_MOUNT_F_EMPTY_PATH,
	); err != nil {
		return nil, fmt.Errorf("mount rewritten %s over %s: %w", filesImageFilename, targetPath, err)
	}

	cleanup := func() error {
		err := ops.unmount(targetPath, unix.MNT_DETACH)
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EINVAL) {
			return nil
		}
		return err
	}

	return cleanup, nil
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package lifecyclehooks

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/ai-dynamo/snapshot/agent/internal/runtime"
)

// ErrExists is returned by CreateMarker when the file is already there. The
// caller decides whether that is its own file from an earlier attempt or one
// it must not accept.
var ErrExists = errors.New("file already exists")

// resolve keeps every lookup inside the container's root and refuses any
// symlink on the way, so a path can't be pointed at the node's filesystem or
// at a volume CheckPath would have refused.
const resolve = unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS

// FileExists reports whether a regular file exists at filePath inside the
// container that hostPID runs in. A missing file, or a missing parent, is
// false. Anything else at the path, a symlink included, is an error, so a
// caller never reads it as either answer.
func FileExists(hostPID int, filePath string) (bool, error) {
	return fileExists(runtime.HostProcPath, hostPID, filePath)
}

// CreateMarker creates an empty regular file at filePath inside the container
// that hostPID runs in, readable by every user. The parent directory must
// exist; it is never created. An existing regular file is left alone and
// reported as ErrExists; anything else at the path is an error.
func CreateMarker(hostPID int, filePath string) error {
	return createMarker(runtime.HostProcPath, hostPID, filePath)
}

func fileExists(procRoot string, hostPID int, filePath string) (bool, error) {
	if err := validatePath(filePath); err != nil {
		return false, err
	}
	root, err := openContainerRoot(procRoot, hostPID)
	if err != nil {
		return false, err
	}
	defer unix.Close(root)
	fd, err := unix.Openat2(root, strings.TrimPrefix(filePath, "/"), &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_NOFOLLOW | unix.O_CLOEXEC,
		Resolve: resolve,
	})
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, fmt.Errorf("open %s in the container: %w", filePath, describe(err))
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return false, fmt.Errorf("stat %s in the container: %w", filePath, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return false, fmt.Errorf("%s in the container is not a regular file", filePath)
	}
	return true, nil
}

func createMarker(procRoot string, hostPID int, filePath string) error {
	if err := validatePath(filePath); err != nil {
		return err
	}
	root, err := openContainerRoot(procRoot, hostPID)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	parent := strings.TrimPrefix(path.Dir(filePath), "/")
	if parent == "" {
		parent = "."
	}
	dir, err := unix.Openat2(root, parent, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: resolve,
	})
	if err != nil {
		return fmt.Errorf("open the directory of %s in the container: %w", filePath, describe(err))
	}
	defer unix.Close(dir)
	fd, err := unix.Openat(dir, path.Base(filePath), unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o644)
	if err != nil {
		if errors.Is(err, unix.EEXIST) {
			return existing(dir, filePath)
		}
		return fmt.Errorf("create %s in the container: %w", filePath, err)
	}
	defer unix.Close(fd)
	// The umask may have narrowed the mode at creation.
	if err := unix.Fchmod(fd, 0o644); err != nil {
		return fmt.Errorf("make %s in the container readable: %w", filePath, err)
	}
	return nil
}

// existing reports ErrExists only for a regular file, so a caller that accepts
// its own earlier marker never accepts a symlink or a directory in its place.
func existing(dir int, filePath string) error {
	var st unix.Stat_t
	if err := unix.Fstatat(dir, path.Base(filePath), &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("stat existing %s in the container: %w", filePath, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("%s in the container exists and is not a regular file", filePath)
	}
	return ErrExists
}

// openContainerRoot opens the root of the container that hostPID runs in, as
// seen through procRoot/<pid>/root.
func openContainerRoot(procRoot string, hostPID int) (int, error) {
	if hostPID <= 0 {
		return -1, fmt.Errorf("invalid host PID %d", hostPID)
	}
	rootPath := filepath.Join(procRoot, strconv.Itoa(hostPID), "root")
	fd, err := unix.Open(rootPath, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open container root %s: %w", rootPath, err)
	}
	return fd, nil
}

// describe names the refusal behind ELOOP, which RESOLVE_NO_SYMLINKS returns
// for any symlink on the path.
func describe(err error) error {
	if errors.Is(err, unix.ELOOP) {
		return fmt.Errorf("the path goes through a symlink, which is not allowed: %w", err)
	}
	return err
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// storeDirPrefix starts the name of a store directory New creates under the
// temporary directory, when Config names none.
const storeDirPrefix = "snapshot-coordinator-"

// newStoreDir returns the directory of the embedded server's file store, which
// holds the control buckets, empty: dir, emptied, or created if it does not
// exist, or a new directory under the temporary directory when dir is empty.
func newStoreDir(dir string) (string, error) {
	if dir == "" {
		dir, err := os.MkdirTemp("", storeDirPrefix)
		if err != nil {
			return "", fmt.Errorf("create the store directory: %w", err)
		}
		return dir, nil
	}
	// A trailing slash or dot makes Lstat follow the final symlink.
	// Normalize before inspecting or emptying the directory.
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create the store directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("inspect the store directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("store directory %q must not be a symlink", dir)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
		return "", fmt.Errorf("store directory %q must have effective uid %d as its owner", dir, os.Geteuid())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("empty the store directory: %w", err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return "", fmt.Errorf("empty the store directory: %w", err)
		}
	}
	return dir, nil
}

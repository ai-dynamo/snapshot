// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"os"
	"path/filepath"
)

// storeDirPrefix starts the name of a store directory New creates under the
// temporary directory, when Config names none.
const storeDirPrefix = "snapshot-coordinator-"

// newStoreDir returns the directory of the embedded server's file store, which
// holds the control buckets, empty: dir, emptied, or created if it does not
// exist, or a new directory under the temporary directory when dir is empty.
// Only its entries are removed, so dir may be a mount point.
func newStoreDir(dir string) (string, error) {
	if dir == "" {
		dir, err := os.MkdirTemp("", storeDirPrefix)
		if err != nil {
			return "", fmt.Errorf("create the store directory: %w", err)
		}
		return dir, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create the store directory: %w", err)
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

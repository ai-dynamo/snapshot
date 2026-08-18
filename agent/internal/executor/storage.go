// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ValidateCheckpointStorage verifies that the checkpoint filesystem supports
// the atomic no-replace rename used to publish immutable artifacts.
func ValidateCheckpointStorage(basePath string) (retErr error) {
	probeDir, err := os.MkdirTemp(basePath, ".snapshot-publish-probe-")
	if err != nil {
		return fmt.Errorf("create checkpoint publication probe: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(probeDir); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("remove checkpoint publication probe: %w", err))
		}
	}()

	stagedDir := filepath.Join(probeDir, "staged")
	finalDir := filepath.Join(probeDir, "final")
	for _, path := range []string{stagedDir, finalDir} {
		if err := os.Mkdir(path, 0o700); err != nil {
			return fmt.Errorf("create checkpoint publication probe directory: %w", err)
		}
	}

	err = renameNoReplace(stagedDir, finalDir)
	if os.IsExist(err) {
		return nil
	}
	if err == nil {
		return fmt.Errorf("checkpoint filesystem at %s replaced an existing publication probe", basePath)
	}
	return fmt.Errorf("checkpoint filesystem at %s does not support atomic no-replace publication: %w", basePath, err)
}

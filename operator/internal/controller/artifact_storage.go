// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// PrepareArtifactStorage validates the operator's destructive-operation root
// and creates the reserved artifacts directory when necessary.
func PrepareArtifactStorage(basePath string) error {
	mountInfo, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("open mountinfo: %w", err)
	}
	defer mountInfo.Close()
	return prepareArtifactStorage(basePath, mountInfo)
}

func prepareArtifactStorage(basePath string, mountInfo io.Reader) error {
	if err := snapshotv1alpha1.ValidateArtifactBasePath(basePath); err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(basePath)
	if err != nil {
		return fmt.Errorf("resolve artifact base path: %w", err)
	}
	if resolved != basePath {
		return fmt.Errorf("artifact base path %q resolves through symlink to %q", basePath, resolved)
	}
	info, err := os.Lstat(basePath)
	if err != nil {
		return fmt.Errorf("inspect artifact base path: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("artifact base path %q must be a non-symlink directory", basePath)
	}
	mounted, err := mountInfoContainsPath(mountInfo, basePath)
	if err != nil {
		return err
	}
	if !mounted {
		return fmt.Errorf("artifact base path %q is not a filesystem mount point", basePath)
	}
	artifactsRoot, err := snapshotv1alpha1.ResolveArtifactsRoot(basePath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(artifactsRoot, 0o750); err != nil {
		return fmt.Errorf("create artifacts root: %w", err)
	}
	rootInfo, err := os.Lstat(artifactsRoot)
	if err != nil {
		return fmt.Errorf("inspect artifacts root: %w", err)
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("artifacts root %q must be a non-symlink directory", artifactsRoot)
	}
	return nil
}

func mountInfoContainsPath(reader io.Reader, target string) (bool, error) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		mountPath, err := unescapeMountInfoField(fields[4])
		if err != nil {
			return false, err
		}
		if mountPath == target {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("read mountinfo: %w", err)
	}
	return false, nil
}

func unescapeMountInfoField(value string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(value); {
		if value[i] != '\\' {
			out.WriteByte(value[i])
			i++
			continue
		}
		if i+3 >= len(value) {
			return "", fmt.Errorf("invalid mountinfo escape in %q", value)
		}
		decoded, err := strconv.ParseUint(value[i+1:i+4], 8, 8)
		if err != nil {
			return "", fmt.Errorf("invalid mountinfo escape in %q: %w", value, err)
		}
		out.WriteByte(byte(decoded))
		i += 4
	}
	return out.String(), nil
}

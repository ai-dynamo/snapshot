// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package backends implements maintenance.Backend for each supported store.
package backends

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ai-dynamo/snapshot/agent/pkg/artifact"
	"github.com/ai-dynamo/snapshot/api/podcontract"
	"github.com/go-logr/logr"
)

// ErrUnsafeArtifact signals a backend refused to touch what it found.
var ErrUnsafeArtifact = errors.New("artifact root is not an ordinary directory")

const NamePVC = "PVC"

// PVCBackend implements maintenance.Backend against a shared PVC mounted at basePath.
type PVCBackend struct {
	basePath string
}

func NewPVCBackend(basePath string) *PVCBackend {
	return &PVCBackend{basePath: basePath}
}

func (b *PVCBackend) Name() string {
	return NamePVC
}

func (b *PVCBackend) Delete(_ context.Context, contentUID string) error {
	return removeArtifactRoot(b.basePath, contentUID)
}

func (b *PVCBackend) Candidates(_ context.Context, logger logr.Logger) (map[string]struct{}, error) {
	return enumerateSweepCandidates(b.basePath, logger)
}

// HelperCandidates enumerates only the reserved SnapshotJob helper namespace.
func (b *PVCBackend) HelperCandidates(_ context.Context, logger logr.Logger) (map[string]struct{}, error) {
	return enumerateOwnedRoots(b.basePath, podcontract.HelperArtifactsDirectory, logger)
}

// DeleteHelpers removes the whole directory of one positively failed attempt.
// The maintenance caller must first prove the writers have stopped.
func (b *PVCBackend) DeleteHelpers(_ context.Context, jobUID string) error {
	if _, err := podcontract.HelperArtifactSubdir(jobUID); err != nil {
		return err
	}
	return removeOwnedRoot(b.basePath, podcontract.HelperArtifactsDirectory, jobUID)
}

// removeArtifactRoot refuses non-ordinary (e.g. symlinked) directories.
func removeArtifactRoot(basePath, contentUID string) error {
	return removeOwnedRoot(basePath, "", contentUID)
}

func resolveOwnedRoot(basePath, directory, uid string) (string, string, error) {
	if err := artifact.ValidateBasePath(basePath); err != nil {
		return "", "", err
	}
	if err := artifact.ValidatePathElement("artifact owner UID", uid); err != nil {
		return "", "", err
	}
	root, err := artifact.ResolveRoot(basePath)
	if err != nil {
		return "", "", err
	}
	if directory != "" {
		if err := artifact.ValidatePathElement("artifact directory", directory); err != nil {
			return "", "", err
		}
		root = filepath.Join(basePath, directory)
	}
	return root, filepath.Join(root, uid), nil
}

func removeOwnedRoot(basePath, directory, uid string) error {
	artifactsRoot, root, err := resolveOwnedRoot(basePath, directory, uid)
	if err != nil {
		return err
	}
	if err := artifact.ValidateDirectory(artifactsRoot); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("%w: %w", ErrUnsafeArtifact, err)
	}
	if err := artifact.ValidateDirectory(root); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %w", ErrUnsafeArtifact, err)
	}
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("remove artifact root %q: %w", root, err)
	}
	return nil
}

// enumerateSweepCandidates skips unsafe entries rather than deleting them.
func enumerateSweepCandidates(basePath string, logger logr.Logger) (map[string]struct{}, error) {
	return enumerateOwnedRoots(basePath, "", logger)
}

func enumerateOwnedRoots(basePath, directory string, logger logr.Logger) (map[string]struct{}, error) {
	artifactsRoot, _, err := resolveOwnedRoot(basePath, directory, "unused")
	if err != nil {
		return nil, err
	}
	if err := artifact.ValidateDirectory(artifactsRoot); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]struct{}{}, nil
		}
		return nil, err
	}
	entries, err := os.ReadDir(artifactsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]struct{}{}, nil
		}
		return nil, fmt.Errorf("enumerate artifact roots: %w", err)
	}
	candidates := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if err := artifact.ValidatePathElement("artifact directory entry", name); err != nil {
			logger.Error(err, "Ignoring unsafe artifact directory entry", "entry", name)
			continue
		}
		_, path, err := resolveOwnedRoot(basePath, directory, name)
		if err != nil {
			logger.Error(err, "Ignoring unresolved artifact directory entry", "entry", name)
			continue
		}
		if err := artifact.ValidateDirectory(path); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				logger.Error(err, "Ignoring unexpected artifact directory entry", "entry", name)
			}
			continue
		}
		candidates[name] = struct{}{}
	}
	return candidates, nil
}

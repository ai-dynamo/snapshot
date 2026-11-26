// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nsmount

import (
	"fmt"
	"path/filepath"

	"github.com/ai-dynamo/snapshot/agent/pkg/artifact"
)

const (
	containersDirectory = "containers"
)

// ResolveArtifactPath returns the checkpoint artifact path owned by one
// PodSnapshotContent and captured container. All variable components must be
// single safe path elements.
func ResolveArtifactPath(basePath, contentUID, containerName string) (string, error) {
	root, err := artifact.ResolveContentRoot(basePath, contentUID)
	if err != nil {
		return "", err
	}
	if err := artifact.ValidatePathElement("container name", containerName); err != nil {
		return "", err
	}
	return filepath.Join(root, containersDirectory, containerName), nil
}

// validateArtifactSource accepts a checkpoint artifact from the snapshot store
// or from the published copies in the node cache, and nothing else.
func validateArtifactSource(source string) error {
	if err := validateWithin(NodeCacheArtifactsSrc, source); err == nil && source != NodeCacheArtifactsSrc {
		return nil
	}
	if err := validateWithin(CheckpointSrc, source); err != nil {
		return fmt.Errorf("%w; it is not a copy below %s either", err, NodeCacheArtifactsSrc)
	}
	return nil
}

func validateWithin(root, source string) error {
	if err := artifact.ValidateContainment(root, source); err != nil {
		return fmt.Errorf("mount source containment: %w", err)
	}
	return nil
}

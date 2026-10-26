// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"fmt"
	"path"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// NonTargetArtifactContainersAnnotation opts named, regular non-target source
	// containers into SnapshotJob-owned non-target artifact storage. The caller
	// mounts the same shared PVC used by Snapshot artifact maintenance.
	NonTargetArtifactContainersAnnotation = "nvidia.com/snapshot-non-target-artifact-containers"
	// SnapshotJobUIDEnv identifies one immutable checkpoint attempt.
	SnapshotJobUIDEnv = "SNAPSHOT_JOB_UID"
	// NonTargetArtifactSubdirEnv names the attempt's directory relative to the
	// shared PVC root, allowing non-target containers and maintenance to use different mounts.
	NonTargetArtifactSubdirEnv  = "SNAPSHOT_NON_TARGET_ARTIFACT_SUBDIR"
	NonTargetArtifactsDirectory = "non-target-artifacts"
)

// NonTargetArtifactSubdir returns the directory owned by one SnapshotJob attempt.
// A UID is validated before it is allowed to become a filesystem path element.
func NonTargetArtifactSubdir(uid string) (string, error) {
	if uid == "" || uid == "." || uid == ".." || len(validation.IsDNS1123Subdomain(uid)) != 0 {
		return "", fmt.Errorf("invalid SnapshotJob UID for non-target artifacts: %q", uid)
	}
	return path.Join(NonTargetArtifactsDirectory, uid), nil
}

// NonTargetArtifactContainers parses the opt-in annotation. Container membership
// and exclusion of the capture target are checked against the source Pod.
func NonTargetArtifactContainers(annotations map[string]string) ([]string, error) {
	raw, present := annotations[NonTargetArtifactContainersAnnotation]
	if !present {
		return nil, nil
	}
	var names []string
	seen := make(map[string]struct{})
	for part := range strings.SplitSeq(raw, ",") {
		name := strings.TrimSpace(part)
		if len(validation.IsDNS1123Label(name)) != 0 {
			return nil, fmt.Errorf("%s contains invalid container name %q", NonTargetArtifactContainersAnnotation, name)
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("%s repeats container %q", NonTargetArtifactContainersAnnotation, name)
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names, nil
}

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
	// HelperArtifactContainersAnnotation opts named, regular non-target source
	// containers into SnapshotJob-owned helper artifact storage. The caller
	// mounts the same shared PVC used by Snapshot artifact maintenance.
	HelperArtifactContainersAnnotation = "nvidia.com/snapshot-helper-artifact-containers"
	// SnapshotJobUIDEnv identifies one immutable checkpoint attempt.
	SnapshotJobUIDEnv = "SNAPSHOT_JOB_UID"
	// HelperArtifactSubdirEnv names the attempt's directory relative to the
	// shared PVC root, allowing helpers and maintenance to use different mounts.
	HelperArtifactSubdirEnv  = "SNAPSHOT_HELPER_ARTIFACT_SUBDIR"
	HelperArtifactsDirectory = "helper-artifacts"
)

// HelperArtifactSubdir returns the directory owned by one SnapshotJob attempt.
// A UID is validated before it is allowed to become a filesystem path element.
func HelperArtifactSubdir(uid string) (string, error) {
	if uid == "" || uid == "." || uid == ".." || len(validation.IsDNS1123Subdomain(uid)) != 0 {
		return "", fmt.Errorf("invalid SnapshotJob UID for helper artifacts: %q", uid)
	}
	return path.Join(HelperArtifactsDirectory, uid), nil
}

// HelperArtifactContainers parses the opt-in annotation. Container membership
// and exclusion of the capture target are checked against the source Pod.
func HelperArtifactContainers(annotations map[string]string) ([]string, error) {
	raw, present := annotations[HelperArtifactContainersAnnotation]
	if !present {
		return nil, nil
	}
	var names []string
	seen := make(map[string]struct{})
	for part := range strings.SplitSeq(raw, ",") {
		name := strings.TrimSpace(part)
		if len(validation.IsDNS1123Label(name)) != 0 {
			return nil, fmt.Errorf("%s contains invalid container name %q", HelperArtifactContainersAnnotation, name)
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("%s repeats container %q", HelperArtifactContainersAnnotation, name)
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names, nil
}

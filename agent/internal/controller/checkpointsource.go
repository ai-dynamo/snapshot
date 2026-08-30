// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/compat"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// checkpointSourceAtPath reads the manifest beside a committed artifact and
// projects it for publication.
func checkpointSourceAtPath(artifactPath string) (*snapshotv1alpha1.CheckpointSource, error) {
	manifest, err := types.ReadManifest(artifactPath)
	if err != nil {
		return nil, fmt.Errorf("read checkpoint manifest for source status: %w", err)
	}
	return checkpointSourceFromManifest(manifest), nil
}

// checkpointSourceFromManifest projects a checkpoint manifest onto the status
// block that publishes it. The compared facts come from CompatFacts, the same
// view the restore gates read, so what a user sees cannot drift from what a
// restore is actually checked against.
//
// A block with nothing to say is left out entirely, and a manifest with nothing
// to say at all projects to nil rather than to an empty object.
func checkpointSourceFromManifest(manifest *types.CheckpointManifest) *snapshotv1alpha1.CheckpointSource {
	if manifest == nil {
		return nil
	}

	facts := manifest.CompatFacts()
	source := &snapshotv1alpha1.CheckpointSource{}

	node := snapshotv1alpha1.CheckpointSourceNode{
		Name:          manifest.K8s.SourceNode,
		Architecture:  facts.CPUArch,
		KernelVersion: facts.KernelVersion,
	}
	if node != (snapshotv1alpha1.CheckpointSourceNode{}) {
		source.Node = &node
	}

	pod := snapshotv1alpha1.CheckpointSourcePod{
		Image:       facts.Image,
		ImageDigest: compat.ImageDigest(facts.ImageID),
		Memory:      facts.MemoryLimit,
		CPU:         facts.CPULimit,
	}
	if pod != (snapshotv1alpha1.CheckpointSourcePod{}) {
		source.Pod = &pod
	}

	if facts.DriverVersion != "" || len(facts.GPUDevices) > 0 {
		nvidia := &snapshotv1alpha1.NvidiaCheckpointSource{DriverVersion: facts.DriverVersion}
		for _, device := range facts.GPUDevices {
			nvidia.Instances = append(nvidia.Instances, snapshotv1alpha1.NvidiaCheckpointSourceInstance{
				ProductName: device.ProductName,
			})
		}
		source.Devices = &snapshotv1alpha1.CheckpointSourceDevices{Nvidia: nvidia}
	}

	if source.Node == nil && source.Pod == nil && source.Devices == nil {
		return nil
	}
	return source
}

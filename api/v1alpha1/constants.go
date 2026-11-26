// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// Snapshot control-plane labels and storage vocabulary shared by the operator
// and node agent. Workload Pod contract constants live in api/podcontract.
const (
	// CaptureEligibleLabel is the gate-applied promotion label: the node agent's pre-bind gate
	// adds it only after the source pod passes validation. The agent's source-pod capture
	// informer keys on it so only gate-validated pods drive the capture path.
	CaptureEligibleLabel = "nvidia.com/snapshot-capture-eligible"

	// SnapshotNodeLabel mirrors PodSnapshotContent.spec.source.nodeName onto the
	// object so the per-node agent's cache can label-select work for its node.
	SnapshotNodeLabel = "nvidia.com/snapshot-node"

	// SnapshotStorageClassLabel names the SnapshotStorageClass a
	// PodSnapshotContent was bound through. It is for queries only.
	SnapshotStorageClassLabel = "nvidia.com/snapshot-storage-class"

	CheckpointVolumeName = "checkpoint-storage"

	// MaxCheckpointRetryLimit bounds every way a retry limit is asked for: the
	// three spec fields and the agent's cluster default. Retries are for
	// conditions that clear on their own, so a number past this one buys nothing
	// and holds the node's staging and its agent for every attempt.
	//
	// The spec fields enforce it with a kubebuilder Maximum marker, which has to
	// be a literal. TestCheckpointRetryLimitMarkerMatchesTheConstant keeps the
	// two from drifting.
	MaxCheckpointRetryLimit = 10
)

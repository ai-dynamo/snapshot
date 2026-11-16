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

	CheckpointVolumeName = "checkpoint-storage"

	// PodGroupLabel holds the name of the PodGroupSnapshot that owns a
	// PodSnapshot, on each PodSnapshot that a PodGroupSnapshot creates for its
	// members.
	PodGroupLabel = "nvidia.com/snapshot-podgroup"

	// PodGroupMemberLabel holds the member ID of a PodSnapshot that a
	// PodGroupSnapshot creates for one of its members.
	PodGroupMemberLabel = "nvidia.com/snapshot-podgroup-member"

	// PodGroupProtocolsAnnotation on the node agent DaemonSet's Pod template,
	// and so on every agent Pod, lists the group protocol versions that the
	// agent supports, as a comma-separated list such as "v1,v2". The group
	// controllers place member work only on nodes whose agent lists a
	// compatible version.
	PodGroupProtocolsAnnotation = "nvidia.com/snapshot-podgroup-protocols"

	// PodGroupInUseFinalizer keeps a group resource, of either kind, while its
	// own attempt runs, and a PodGroupSnapshot, with the snapshots of its
	// members, also while any unfinished PodGroupRestore references its UID.
	PodGroupInUseFinalizer = "nvidia.com/snapshot-podgroup-in-use"
)

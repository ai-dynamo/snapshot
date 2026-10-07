// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package maintenance runs PodSnapshotContent artifact cleanup through an
// in-process, rate-limiting workqueue instead of per-task Kubernetes Jobs.
package maintenance

import "k8s.io/apimachinery/pkg/types"

// PodSnapshotContentArtifactCleanupFinalizer blocks deletion until artifacts are removed.
const PodSnapshotContentArtifactCleanupFinalizer = "nvidia.com/podsnapshotcontent-artifact-cleanup"

// ArtifactCleanupBlockedReason is the Warning Event reason for an unsafe artifact root.
const ArtifactCleanupBlockedReason = "ArtifactCleanupBlocked"

// Mode selects which maintenance operation a work item performs.
type Mode string

const (
	ModeDeleteContent   Mode = "delete-content"
	ModeSweep           Mode = "sweep"
	ModeRecoverMetadata Mode = "recover-metadata"
)

// Enqueuer is the subset of Queue used to schedule work, so callers (and
// their tests) don't depend on the workqueue implementation directly.
type Enqueuer interface {
	EnqueueDeleteContent(name string, uid types.UID)
	EnqueueRecoverMetadata(name string, uid types.UID, storeID, containerName string)
}

// WorkItemKey identifies one unit of maintenance work; it is comparable so
// the workqueue deduplicates repeated enqueues.
//
// Name/UID matter only for ModeDeleteContent and ModeRecoverMetadata: the
// worker re-reads the object and compares UID, so a content deleted and
// recreated under the same name never matches a stale key. ModeSweep
// carries no identity, so duplicate sweep triggers coalesce into one key.
// StoreID/ContainerName narrow ModeRecoverMetadata to the exact publication
// a worker resolves store configuration for at processing time; a store-ID
// mismatch there is a terminal refusal, not a retry.
type WorkItemKey struct {
	Mode          Mode
	Name          string
	UID           types.UID
	StoreID       string
	ContainerName string
}

func newDeleteContentKey(name string, uid types.UID) WorkItemKey {
	return WorkItemKey{Mode: ModeDeleteContent, Name: name, UID: uid}
}

func newSweepKey() WorkItemKey {
	return WorkItemKey{Mode: ModeSweep}
}

func newRecoverMetadataKey(name string, uid types.UID, storeID, containerName string) WorkItemKey {
	return WorkItemKey{Mode: ModeRecoverMetadata, Name: name, UID: uid, StoreID: storeID, ContainerName: containerName}
}

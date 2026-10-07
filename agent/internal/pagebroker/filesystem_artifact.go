// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import "path"

// FilesystemFormatVersion is the format the PVC artifact store publishes.
const FilesystemFormatVersion = "snapshot.pagebroker/v1"

// FilesystemArtifact is the descriptor the PVC artifact store derives for a
// capture target: artifacts/<contentUID>/containers/<name> in the bound store.
// It exists so a capture whose Commit reply was lost can be looked up again;
// restores always use the descriptor recorded in status, never this.
func FilesystemArtifact(storeID, contentUID, containerName string) *PublishedArtifact {
	return &PublishedArtifact{
		StoreId:               storeID,
		ArtifactHandle:        path.Join("artifacts", contentUID, "containers", containerName),
		ArtifactFormatVersion: FilesystemFormatVersion,
	}
}

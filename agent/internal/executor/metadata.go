// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"

	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

// FetchArtifactManifest reads a publication's manifest through PageBroker's
// metadata transaction, releasing it on every exit.
func FetchArtifactManifest(ctx context.Context, controlSocketPath string, artifact *pagebroker.PublishedArtifact) (*types.CheckpointManifest, error) {
	return fetchArtifactMetadata(ctx, pagebroker.Client{ControlSocketPath: controlSocketPath}, artifact)
}

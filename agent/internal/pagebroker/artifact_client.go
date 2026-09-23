// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"context"
	"fmt"
	"time"
)

const metadataRequestTimeout = 2 * time.Minute

// PrepareArtifactCheckpoint validates a bound destination before capture. A nil
// engine selects the broker's configured default; it never changes the store.
func (c Client) PrepareArtifactCheckpoint(ctx context.Context, transactionID string, target *ArtifactTarget, engine *IOEngine) (string, error) {
	if target.GetStoreId() == "" || target.GetArtifact().GetArtifactUid() == "" || target.GetArtifact().GetContainerName() == "" {
		return "", fmt.Errorf("PageBroker artifact target requires store ID, artifact UID and container name")
	}
	response, err := c.request(ctx, transactionID, &Request_PrepareStagedCheckpoint{
		PrepareStagedCheckpoint: &PrepareStagedCheckpointRequest{Target: target, IoEngine: engine},
	})
	if err != nil {
		return "", err
	}
	return imageDirectory(response.GetStagedCheckpointDirectory().GetImageDirectory())
}

// CommitCheckpoint returns the descriptor of a target-addressed checkpoint.
// Retries preserve the transaction ID and result. Legacy checkpoints and restore
// cleanup use Commit. A missing result never authorizes another capture attempt.
func (c Client) CommitCheckpoint(ctx context.Context, transactionID string) (*PublishedArtifact, error) {
	result, err := c.commitWithRetry(ctx, transactionID)
	if err != nil {
		return nil, err
	}
	artifact := result.GetPublishedArtifact()
	if err := validatePublishedArtifact(artifact); err != nil {
		return nil, fmt.Errorf("invalid PageBroker checkpoint Commit result: %w", err)
	}
	return artifact, nil
}

// StagedArtifactRestore verifies the exact publication into local staging. A nil
// engine selects the broker's configured default. Finish consumers and unmount
// before Commit or Abort releases staging; neither deletes the publication.
func (c Client) StagedArtifactRestore(ctx context.Context, transactionID string, artifact *PublishedArtifact, engine *IOEngine) (string, error) {
	if err := validatePublishedArtifact(artifact); err != nil {
		return "", err
	}
	response, err := c.request(ctx, transactionID, &Request_StagedRestore{
		StagedRestore: &StagedRestoreRequest{Artifact: artifact, IoEngine: engine},
	})
	if err != nil {
		return "", err
	}
	return imageDirectory(response.GetStagedRestoreDirectory().GetImageDirectory())
}

// GetArtifactMetadata returns a private shared directory with verified
// manifest.yaml, not a verified payload. The caller must Abort on every exit,
// including a lost reply, using a bounded cleanup context if ctx was cancelled.
func (c Client) GetArtifactMetadata(ctx context.Context, transactionID string, artifact *PublishedArtifact) (string, error) {
	if err := validatePublishedArtifact(artifact); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, metadataRequestTimeout)
	defer cancel()
	response, err := c.request(ctx, transactionID, &Request_GetArtifactMetadata{
		GetArtifactMetadata: &GetArtifactMetadataRequest{Artifact: artifact},
	})
	if err != nil {
		return "", err
	}
	directory := response.GetGetArtifactMetadataComplete().GetManifestDirectory()
	if directory == "" {
		return "", fmt.Errorf("unexpected PageBroker metadata response")
	}
	return directory, nil
}

func validatePublishedArtifact(artifact *PublishedArtifact) error {
	if artifact.GetStoreId() == "" || artifact.GetArtifactHandle() == "" || artifact.GetArtifactFormatVersion() == "" {
		return fmt.Errorf("PageBroker publication requires store ID, artifact handle and format version")
	}
	return nil
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
)

const metadataRequestTimeout = 2 * time.Minute

// PrepareArtifactCheckpoint validates a bound destination before capture. A nil
// engine selects the broker's configured default; it never changes the store.
func (c Client) PrepareArtifactCheckpoint(ctx context.Context, transactionID string, target *ArtifactTarget, engine *IOEngine) (string, error) {
	if err := validateArtifactTarget(target); err != nil {
		return "", err
	}
	response, err := c.request(ctx, transactionID, &Request_PrepareStagedCheckpoint{
		PrepareStagedCheckpoint: &PrepareStagedCheckpointRequest{
			Selector: &PrepareStagedCheckpointRequest_Target{Target: target},
			IoEngine: engine,
		},
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
		StagedRestore: &StagedRestoreRequest{
			Selector: &StagedRestoreRequest_Artifact{Artifact: artifact},
			IoEngine: engine,
		},
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
	result, err := c.artifactMetadata(
		ctx, transactionID, &GetArtifactMetadataRequest{Selector: &GetArtifactMetadataRequest_Artifact{Artifact: artifact}})
	if err != nil {
		return "", err
	}
	if !proto.Equal(result.GetArtifact(), artifact) {
		return "", fmt.Errorf("PageBroker metadata names another publication")
	}
	return result.GetManifestDirectory(), nil
}

// LookupArtifactMetadata resolves the publication for a target whose descriptor
// was never received, such as after a lost Commit reply, and stages its manifest
// like GetArtifactMetadata. A missing publication is a FailureError with code
// ARTIFACT_NOT_FOUND. It reads only and never completes a publication.
func (c Client) LookupArtifactMetadata(ctx context.Context, transactionID string, target *ArtifactTarget) (*PublishedArtifact, string, error) {
	if err := validateArtifactTarget(target); err != nil {
		return nil, "", err
	}
	result, err := c.artifactMetadata(
		ctx, transactionID, &GetArtifactMetadataRequest{Selector: &GetArtifactMetadataRequest_Target{Target: target}})
	if err != nil {
		return nil, "", err
	}
	if result.GetArtifact().GetStoreId() != target.GetStoreId() {
		return nil, "", fmt.Errorf("PageBroker metadata names another store")
	}
	return result.GetArtifact(), result.GetManifestDirectory(), nil
}

func (c Client) artifactMetadata(ctx context.Context, transactionID string, request *GetArtifactMetadataRequest) (*GetArtifactMetadataComplete, error) {
	ctx, cancel := context.WithTimeout(ctx, metadataRequestTimeout)
	defer cancel()
	response, err := c.request(ctx, transactionID, &Request_GetArtifactMetadata{GetArtifactMetadata: request})
	if err != nil {
		return nil, err
	}
	result := response.GetGetArtifactMetadataComplete()
	if result.GetManifestDirectory() == "" {
		return nil, fmt.Errorf("unexpected PageBroker metadata response")
	}
	if err := validatePublishedArtifact(result.GetArtifact()); err != nil {
		return nil, fmt.Errorf("invalid PageBroker metadata response: %w", err)
	}
	return result, nil
}

func validateArtifactTarget(target *ArtifactTarget) error {
	if target.GetStoreId() == "" || target.GetArtifact().GetArtifactUid() == "" || target.GetArtifact().GetContainerName() == "" {
		return fmt.Errorf("PageBroker artifact target requires store ID, artifact UID and container name")
	}
	return nil
}

func validatePublishedArtifact(artifact *PublishedArtifact) error {
	if artifact.GetStoreId() == "" || artifact.GetArtifactHandle() == "" {
		return fmt.Errorf("PageBroker publication requires store ID and artifact handle")
	}
	return nil
}

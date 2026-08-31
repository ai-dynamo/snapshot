// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/ai-dynamo/snapshot/agent/pkg/artifact"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	operatortypes "github.com/ai-dynamo/snapshot/operator/internal/types"
	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const podSnapshotContentMetadataListPageLimit int64 = 500

// AddArtifactOrphanScanner registers the scanner with the manager's existing
// leader-election group. RunnableFunc is leader-elected by default.
func AddArtifactOrphanScanner(mgr ctrl.Manager, cfg operatortypes.ArtifactCleanupConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	scanner := artifactOrphanScanner{apiReader: mgr.GetAPIReader(), config: cfg}
	return mgr.Add(manager.RunnableFunc(scanner.run))
}

type artifactOrphanScanner struct {
	apiReader client.Reader
	config    operatortypes.ArtifactCleanupConfig
}

func (s *artifactOrphanScanner) run(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("artifact-orphan-scanner")
	s.scanAndLog(ctx, logger)
	ticker := time.NewTicker(s.config.ScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.scanAndLog(ctx, logger)
		}
	}
}

func (s *artifactOrphanScanner) scanAndLog(ctx context.Context, logger logr.Logger) {
	if err := s.scanOnce(ctx, logger); err != nil {
		logger.Error(err, "Artifact orphan scan failed; remaining candidates were left in place")
	}
}

func (s *artifactOrphanScanner) scanOnce(ctx context.Context, logger logr.Logger) error {
	candidates, err := s.enumerateCandidates(logger)
	if err != nil {
		return err
	}
	existing, err := s.listExistingUIDs(ctx)
	if err != nil {
		return err
	}
	var scanErrors []error
	processed := 0
	for uid := range candidates {
		if _, protected := existing[types.UID(uid)]; protected {
			continue
		}
		if processed == s.config.BatchSize {
			break
		}
		processed++
		if err := removeArtifactRoot(s.config.BasePath, uid); err != nil {
			scanErrors = append(scanErrors, err)
			logger.Error(err, "Unable to reclaim orphan PodSnapshotContent artifact root", "content_uid", uid)
			continue
		}
		logger.Info("Reclaimed orphan PodSnapshotContent artifact root", "content_uid", uid)
	}
	return errors.Join(scanErrors...)
}

func (s *artifactOrphanScanner) enumerateCandidates(logger logr.Logger) (map[string]struct{}, error) {
	artifactsRoot, err := artifact.ResolveRoot(s.config.BasePath)
	if err != nil {
		return nil, err
	}
	if err := artifact.ValidateDirectory(artifactsRoot); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]struct{}{}, nil
		}
		return nil, err
	}
	entries, err := os.ReadDir(artifactsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]struct{}{}, nil
		}
		return nil, fmt.Errorf("enumerate artifact roots: %w", err)
	}
	candidates := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if err := artifact.ValidatePathElement("artifact directory entry", name); err != nil {
			logger.Error(err, "Ignoring unsafe artifact directory entry", "entry", name)
			continue
		}
		path, err := artifact.ResolveContentRoot(s.config.BasePath, name)
		if err != nil {
			logger.Error(err, "Ignoring unresolved artifact directory entry", "entry", name)
			continue
		}
		if err := artifact.ValidateDirectory(path); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				logger.Error(err, "Ignoring unexpected artifact directory entry", "entry", name)
			}
			continue
		}
		candidates[name] = struct{}{}
	}
	return candidates, nil
}

func (s *artifactOrphanScanner) listExistingUIDs(ctx context.Context) (map[types.UID]struct{}, error) {
	var lastErr error
	for attempt := 1; attempt <= s.config.ListAttempts; attempt++ {
		uids, err := s.listExistingUIDsOnce(ctx)
		if err == nil {
			return uids, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("list PodSnapshotContent metadata failed after %d attempts: %w", s.config.ListAttempts, lastErr)
}

func (s *artifactOrphanScanner) listExistingUIDsOnce(ctx context.Context) (map[types.UID]struct{}, error) {
	uids := make(map[types.UID]struct{})
	continueToken := ""
	snapshotResourceVersion := ""
	for {
		list := &metav1.PartialObjectMetadataList{}
		list.SetGroupVersionKind(snapshotv1alpha1.GroupVersion.WithKind("PodSnapshotContentList"))
		options := &client.ListOptions{Raw: &metav1.ListOptions{Limit: podSnapshotContentMetadataListPageLimit, Continue: continueToken, ResourceVersion: ""}}
		if err := s.apiReader.List(ctx, list, options); err != nil {
			return nil, err
		}
		if snapshotResourceVersion == "" {
			snapshotResourceVersion = list.ResourceVersion
			if snapshotResourceVersion == "" {
				return nil, errors.New("PodSnapshotContent metadata list returned empty resource version")
			}
		} else if list.ResourceVersion != snapshotResourceVersion {
			return nil, fmt.Errorf("PodSnapshotContent metadata list resource version changed from %q to %q", snapshotResourceVersion, list.ResourceVersion)
		}
		for i := range list.Items {
			if list.Items[i].UID == "" {
				return nil, fmt.Errorf("PodSnapshotContent %q returned without UID", list.Items[i].Name)
			}
			uids[list.Items[i].UID] = struct{}{}
		}
		if list.Continue == "" {
			return uids, nil
		}
		if list.Continue == continueToken {
			return nil, fmt.Errorf("PodSnapshotContent metadata list repeated continuation token %q", list.Continue)
		}
		continueToken = list.Continue
	}
}

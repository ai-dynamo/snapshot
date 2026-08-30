// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// ArtifactOrphanScanner authoritatively reclaims artifact roots whose owning
// PodSnapshotContent UID no longer exists.
type ArtifactOrphanScanner struct {
	NonCacheReadClient client.Reader
	Config             ArtifactCleanupConfig
	ReadDir            func(string) ([]os.DirEntry, error)
	Lstat              func(string) (os.FileInfo, error)
	RemoveAll          func(string) error
	Now                func() time.Time

	observed  map[string]time.Time
	reclaimed map[string]struct{}
	cursor    string
}

func (s *ArtifactOrphanScanner) NeedLeaderElection() bool { return true }

func (s *ArtifactOrphanScanner) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("artifact-orphan-scanner")
	s.runAndLog(ctx, logger)
	ticker := time.NewTicker(s.Config.ScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.runAndLog(ctx, logger)
		}
	}
}

func (s *ArtifactOrphanScanner) runAndLog(ctx context.Context, logger logr.Logger) {
	if err := s.ScanOnce(ctx, logger); err != nil {
		logger.Error(err, "Artifact orphan scan failed; remaining candidates were left in place")
	}
}

// ScanOnce performs one directory-first scan. It is exported for deterministic
// tests and diagnostics; the manager calls it serially from Start.
func (s *ArtifactOrphanScanner) ScanOnce(ctx context.Context, logger logr.Logger) error {
	if s.NonCacheReadClient == nil {
		return errors.New("artifact orphan scanner requires a non-cache read client")
	}
	if err := s.Config.Validate(); err != nil {
		return err
	}
	candidates, err := s.enumerateCandidates(logger)
	if err != nil {
		return err
	}
	existing, err := s.listExistingUIDs(ctx)
	if err != nil {
		return err
	}
	if s.observed == nil {
		s.observed = make(map[string]time.Time)
	}
	if s.reclaimed == nil {
		s.reclaimed = make(map[string]struct{})
	}

	present := make(map[string]struct{}, len(candidates))
	for _, uid := range candidates {
		present[uid] = struct{}{}
	}
	for uid := range s.observed {
		if _, ok := present[uid]; !ok {
			delete(s.observed, uid)
		}
	}

	now := s.now()()
	matured := make([]string, 0, len(candidates))
	for _, uid := range candidates {
		if _, protected := existing[types.UID(uid)]; protected {
			delete(s.observed, uid)
			delete(s.reclaimed, uid)
			continue
		}
		first, seen := s.observed[uid]
		if !seen {
			s.observed[uid] = now
			if _, recurring := s.reclaimed[uid]; recurring {
				logger.Error(errors.New("reclaimed artifact root reappeared"), "Artifact root was recreated after its content identity disappeared", "content_uid", uid)
			}
			continue
		}
		if now.Sub(first) >= s.Config.OrphanGrace {
			matured = append(matured, uid)
		}
	}

	selected := selectFairCandidates(matured, s.cursor, s.Config.BatchSize)
	var scanErrors []error
	for _, uid := range selected {
		s.cursor = uid
		root, targetErr := validateArtifactRemovalTarget(s.Config.BasePath, uid, s.lstat())
		if targetErr != nil {
			scanErrors = append(scanErrors, targetErr)
			logger.Error(targetErr, "Refusing unsafe orphan artifact candidate", "content_uid", uid)
			continue
		}
		if removeErr := s.removeAll()(root); removeErr != nil {
			scanErrors = append(scanErrors, fmt.Errorf("remove orphan artifact root %q: %w", root, removeErr))
			continue
		}
		if info, statErr := s.lstat()(root); statErr == nil {
			scanErrors = append(scanErrors, fmt.Errorf("orphan artifact root %q still exists with mode %s", root, info.Mode()))
			continue
		} else if !os.IsNotExist(statErr) {
			scanErrors = append(scanErrors, fmt.Errorf("confirm orphan artifact root %q absent: %w", root, statErr))
			continue
		}
		delete(s.observed, uid)
		s.reclaimed[uid] = struct{}{}
		logger.Info("Reclaimed orphan PodSnapshotContent artifact root", "content_uid", uid, "path", root)
	}
	return errors.Join(scanErrors...)
}

func (s *ArtifactOrphanScanner) enumerateCandidates(logger logr.Logger) ([]string, error) {
	artifactsRoot, err := snapshotv1alpha1.ResolveArtifactsRoot(s.Config.BasePath)
	if err != nil {
		return nil, err
	}
	entries, err := s.readDir()(artifactsRoot)
	if err != nil {
		return nil, fmt.Errorf("enumerate artifact roots: %w", err)
	}
	candidates := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if err := snapshotv1alpha1.ValidateArtifactPathElement("artifact directory entry", name); err != nil {
			logger.Error(err, "Ignoring unsafe artifact directory entry", "entry", name)
			continue
		}
		path, err := snapshotv1alpha1.ResolveArtifactRoot(s.Config.BasePath, name)
		if err != nil {
			logger.Error(err, "Ignoring unresolved artifact directory entry", "entry", name)
			continue
		}
		info, err := s.lstat()(path)
		if err != nil {
			if !os.IsNotExist(err) {
				logger.Error(err, "Unable to inspect artifact directory entry", "entry", name)
			}
			continue
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			logger.Error(errors.New("entry is not an ordinary directory"), "Ignoring unexpected artifact directory entry", "entry", name, "mode", info.Mode())
			continue
		}
		candidates = append(candidates, name)
	}
	sort.Strings(candidates)
	return candidates, nil
}

func (s *ArtifactOrphanScanner) listExistingUIDs(ctx context.Context) (map[types.UID]struct{}, error) {
	var lastErr error
	for attempt := 1; attempt <= s.Config.ListAttempts; attempt++ {
		uids, err := s.listExistingUIDsOnce(ctx)
		if err == nil {
			return uids, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("list PodSnapshotContent metadata failed after %d attempts: %w", s.Config.ListAttempts, lastErr)
}

func (s *ArtifactOrphanScanner) listExistingUIDsOnce(ctx context.Context) (map[types.UID]struct{}, error) {
	uids := make(map[types.UID]struct{})
	continueToken := ""
	snapshotResourceVersion := ""
	for {
		list := &metav1.PartialObjectMetadataList{}
		list.SetGroupVersionKind(podSnapshotContentGVK.GroupVersion().WithKind("PodSnapshotContentList"))
		options := &client.ListOptions{Raw: &metav1.ListOptions{
			Limit:           s.Config.PageLimit,
			Continue:        continueToken,
			ResourceVersion: "",
		}}
		if err := s.NonCacheReadClient.List(ctx, list, options); err != nil {
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
			uid := list.Items[i].UID
			if uid == "" {
				return nil, fmt.Errorf("PodSnapshotContent %q returned without UID", list.Items[i].Name)
			}
			uids[uid] = struct{}{}
		}
		next := list.Continue
		if next == "" {
			return uids, nil
		}
		if next == continueToken {
			return nil, fmt.Errorf("PodSnapshotContent metadata list repeated continuation token %q", next)
		}
		continueToken = next
	}
}

func selectFairCandidates(sorted []string, cursor string, limit int) []string {
	if len(sorted) == 0 || limit <= 0 {
		return nil
	}
	start := sort.SearchStrings(sorted, cursor)
	for start < len(sorted) && sorted[start] <= cursor {
		start++
	}
	if start == len(sorted) {
		start = 0
	}
	count := min(limit, len(sorted))
	selected := make([]string, 0, count)
	for i := 0; i < count; i++ {
		selected = append(selected, sorted[(start+i)%len(sorted)])
	}
	return selected
}

func (s *ArtifactOrphanScanner) readDir() func(string) ([]os.DirEntry, error) {
	if s.ReadDir != nil {
		return s.ReadDir
	}
	return os.ReadDir
}

func (s *ArtifactOrphanScanner) lstat() func(string) (os.FileInfo, error) {
	if s.Lstat != nil {
		return s.Lstat
	}
	return os.Lstat
}

func (s *ArtifactOrphanScanner) removeAll() func(string) error {
	if s.RemoveAll != nil {
		return s.RemoveAll
	}
	return os.RemoveAll
}

func (s *ArtifactOrphanScanner) now() func() time.Time {
	if s.Now != nil {
		return s.Now
	}
	return time.Now
}

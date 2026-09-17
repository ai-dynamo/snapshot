// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package maintenance

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/ai-dynamo/snapshot/agent/pkg/artifact"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const podSnapshotContentMetadataListPageLimit int64 = 500

// errUnsafeArtifactRoot means deletion stopped before touching the
// filesystem; the finalizer is retained.
var errUnsafeArtifactRoot = errors.New("artifact root is not an ordinary directory")

// removeArtifactRoot refuses to touch anything unless the artifacts root and
// the content root both validate as ordinary (non-symlink) directories.
func removeArtifactRoot(basePath, contentUID string) error {
	artifactsRoot, err := artifact.ResolveRoot(basePath)
	if err != nil {
		return err
	}
	if err := artifact.ValidateDirectory(artifactsRoot); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("%w: %w", errUnsafeArtifactRoot, err)
	}
	root, err := artifact.ResolveContentRoot(basePath, contentUID)
	if err != nil {
		return err
	}
	if err := artifact.ValidateDirectory(root); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %w", errUnsafeArtifactRoot, err)
	}
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("remove artifact root %q: %w", root, err)
	}
	return nil
}

// enumerateSweepCandidates lists on-disk content UIDs under basePath's
// artifacts root; unsafe entries are logged and skipped, not deleted.
func enumerateSweepCandidates(basePath string, logger logr.Logger) (map[string]struct{}, error) {
	artifactsRoot, err := artifact.ResolveRoot(basePath)
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
		path, err := artifact.ResolveContentRoot(basePath, name)
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

// listExistingContentUIDs fails closed (deletes nothing) if the resource
// version drifts or a continuation token repeats across pages, since either
// means a concurrent write raced the list.
func listExistingContentUIDs(ctx context.Context, apiReader client.Reader, listAttempts int) (map[types.UID]struct{}, error) {
	var lastErr error
	for attempt := 1; attempt <= listAttempts; attempt++ {
		uids, err := listExistingContentUIDsOnce(ctx, apiReader)
		if err == nil {
			return uids, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("list PodSnapshotContent metadata failed after %d attempts: %w", listAttempts, lastErr)
}

func listExistingContentUIDsOnce(ctx context.Context, apiReader client.Reader) (map[types.UID]struct{}, error) {
	uids := make(map[types.UID]struct{})
	continueToken := ""
	snapshotResourceVersion := ""
	for {
		list := &metav1.PartialObjectMetadataList{}
		list.SetGroupVersionKind(snapshotv1alpha1.GroupVersion.WithKind("PodSnapshotContentList"))
		options := &client.ListOptions{
			Limit:    podSnapshotContentMetadataListPageLimit,
			Continue: continueToken,
			Raw:      &metav1.ListOptions{ResourceVersion: ""},
		}
		if err := apiReader.List(ctx, list, options); err != nil {
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

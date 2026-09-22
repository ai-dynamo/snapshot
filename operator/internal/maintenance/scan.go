// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package maintenance

import (
	"context"
	"errors"
	"fmt"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const podSnapshotContentMetadataListPageLimit int64 = 500

type contentScanResult struct {
	ExistingUIDs   map[types.UID]struct{}
	PendingDeletes []WorkItemKey
}

// collectContentScanResult returns only a complete, consistent metadata snapshot.
func collectContentScanResult(ctx context.Context, apiReader client.Reader, listAttempts int) (*contentScanResult, error) {
	var lastErr error
	for attempt := 1; attempt <= listAttempts; attempt++ {
		scanResult, err := collectContentScanResultOnce(ctx, apiReader)
		if err == nil {
			return scanResult, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("list PodSnapshotContent metadata failed after %d attempts: %w", listAttempts, lastErr)
}

func collectContentScanResultOnce(ctx context.Context, apiReader client.Reader) (*contentScanResult, error) {
	scanResult := &contentScanResult{ExistingUIDs: make(map[types.UID]struct{})}
	continueToken := ""
	snapshotResourceVersion := ""
	for {
		list := &metav1.PartialObjectMetadataList{}
		list.SetGroupVersionKind(snapshotv1alpha1.GroupVersion.WithKind(snapshotv1alpha1.KindPodSnapshotContentList))
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
			content := &list.Items[i]
			if content.UID == "" {
				return nil, fmt.Errorf("PodSnapshotContent %q returned without UID", content.Name)
			}
			scanResult.ExistingUIDs[content.UID] = struct{}{}
			if !content.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(content, PodSnapshotContentArtifactCleanupFinalizer) {
				scanResult.PendingDeletes = append(scanResult.PendingDeletes, newDeleteContentKey(content.Namespace, content.Name, content.UID))
			}
		}
		if list.Continue == "" {
			return scanResult, nil
		}
		if list.Continue == continueToken {
			return nil, fmt.Errorf("PodSnapshotContent metadata list repeated continuation token %q", list.Continue)
		}
		continueToken = list.Continue
	}
}

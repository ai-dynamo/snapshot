// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package maintenance

import (
	"context"
	"testing"

	"github.com/ai-dynamo/snapshot/api/storage/coordination"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func boundTestContent(t *testing.T, name, uid string) *snapshotv1alpha1.PodSnapshotContent {
	t.Helper()
	return &snapshotv1alpha1.PodSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid), ResourceVersion: "1"},
		Spec: snapshotv1alpha1.PodSnapshotContentSpec{
			Storage: &snapshotv1alpha1.CheckpointStorageBinding{
				StoreID: "store-v1-" + fixedHex(),
			},
		},
	}
}

// fixedHex returns a syntactically valid 64-hex-character store ID suffix.
func fixedHex() string {
	const hex = "0123456789abcdef"
	out := make([]byte, 64)
	for i := range out {
		out[i] = hex[i%len(hex)]
	}
	return string(out)
}

func TestRepairPublicationAppliesMatchingEvidence(t *testing.T) {
	base := t.TempDir()
	content := boundTestContent(t, "content", "uid-repair")
	q, _ := newTestQueue(t, base, content)

	commitID, err := coordination.ValidateBinding(content.Spec.Storage.StoreID, string(content.UID), "main")
	require.NoError(t, err)
	found := []coordination.Evidence{{CommitID: commitID, ArtifactHandle: "artifacts/uid-repair/main", FormatVersion: "snapshot.pagebroker/v1"}}

	require.NoError(t, q.RepairPublication(context.Background(), content, "main", found))

	current := &snapshotv1alpha1.PodSnapshotContent{}
	require.NoError(t, q.client.Get(context.Background(), client.ObjectKey{Name: "content"}, current))
	require.NotNil(t, current.Status.Storage)
	require.Len(t, current.Status.Storage.Artifacts, 1)
	require.Equal(t, "artifacts/uid-repair/main", current.Status.Storage.Artifacts[0].ArtifactHandle)
}

func TestRepairPublicationRefusesConflictingEvidence(t *testing.T) {
	base := t.TempDir()
	content := boundTestContent(t, "content", "uid-conflict")
	q, _ := newTestQueue(t, base, content)

	commitID, err := coordination.ValidateBinding(content.Spec.Storage.StoreID, string(content.UID), "main")
	require.NoError(t, err)
	found := []coordination.Evidence{
		{CommitID: commitID, ArtifactHandle: "handle-1", FormatVersion: "v1"},
		{CommitID: commitID, ArtifactHandle: "handle-2", FormatVersion: "v1"},
	}

	err = q.RepairPublication(context.Background(), content, "main", found)
	require.ErrorIs(t, err, coordination.ErrConflictingEvidence)
}

func TestRepairPublicationRefusesUnboundContent(t *testing.T) {
	base := t.TempDir()
	content := &snapshotv1alpha1.PodSnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "legacy", UID: "uid-legacy"}}
	q, _ := newTestQueue(t, base, content)

	err := q.RepairPublication(context.Background(), content, "main", nil)
	require.Error(t, err)
}

func TestMetadataRepairRefusesAnotherConfiguredStore(t *testing.T) {
	content := boundTestContent(t, "content", "uid-other-store")
	q, _ := newTestQueue(t, t.TempDir(), content)
	q.config.StoreID = ""
	key := newRecoverMetadataKey(content.Name, content.UID, content.Spec.Storage.StoreID, "main")

	err := q.processRecoverMetadata(context.Background(), key)
	require.ErrorIs(t, err, coordination.ErrStoreMismatch)
	err = q.RepairPublication(context.Background(), content, "main", nil)
	require.ErrorIs(t, err, coordination.ErrStoreMismatch)
	current := &snapshotv1alpha1.PodSnapshotContent{}
	require.NoError(t, q.client.Get(context.Background(), client.ObjectKeyFromObject(content), current))
	require.Nil(t, current.Status.Storage)
}

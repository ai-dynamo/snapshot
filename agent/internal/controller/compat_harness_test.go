// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/compat"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

const gatedRestoreContainer = "main"

type comparisonCall struct {
	gate   compat.Gate
	source compat.Facts
	target compat.Facts
}

// comparisonSpy stands in for the policy table so a test can decide the verdict
// while the table itself is still being filled in.
type comparisonSpy struct {
	mismatches []compat.Mismatch
	calls      []comparisonCall
}

func (s *comparisonSpy) compare(gate compat.Gate, source, target compat.Facts) []compat.Mismatch {
	s.calls = append(s.calls, comparisonCall{gate: gate, source: source, target: target})
	return s.mismatches
}

// gatedRestore is one restore driven through the compatibility gates: a
// controller, the restore pod, the snapshot it names, the artifact it resolves
// to, and a stubbed comparison. Every test about what a gate does starts from
// one of these, whether the verdict it forces lets the restore through or not.
type gatedRestore struct {
	controller *NodeController
	pod        *corev1.Pod
	artifact   *restoreArtifact
	comparison *comparisonSpy
}

func newGatedRestore(t *testing.T, mismatches ...compat.Mismatch) *gatedRestore {
	t.Helper()
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	snapshot, content := readySnapshotObjects()
	w := makeTestController(t, pod, snapshot, content)
	comparison := &comparisonSpy{mismatches: mismatches}
	w.compareFn = comparison.compare

	path := writeTestArtifact(t, w.config.Storage.BasePath, string(content.UID), &types.CheckpointManifest{
		Artifact: types.ArtifactManifest{ContentUID: string(content.UID), ContainerName: gatedRestoreContainer},
	})

	return &gatedRestore{
		controller: w,
		pod:        pod,
		artifact: &restoreArtifact{
			SnapshotName:        snapshot.Name,
			ContentUID:          string(content.UID),
			SourceContainerName: gatedRestoreContainer,
			Path:                path,
		},
		comparison: comparison,
	}
}

// writeTestArtifact creates the artifact directory a restore pod resolves to,
// optionally with a manifest in it.
func writeTestArtifact(t *testing.T, basePath, contentUID string, manifest *types.CheckpointManifest) string {
	t.Helper()
	path, err := nsmount.ResolveArtifactPath(basePath, contentUID, gatedRestoreContainer)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(path, 0o700))
	if manifest != nil {
		require.NoError(t, types.WriteManifest(path, manifest))
	}
	return path
}

// reconcile drives the restore the way the queue worker does.
func (r *gatedRestore) reconcile(t *testing.T) {
	t.Helper()
	processQueuedRestorePod(t, r.controller, r.pod)
}

func (r *gatedRestore) clientset(t *testing.T) *fake.Clientset {
	t.Helper()
	clientset, ok := r.controller.clientset.(*fake.Clientset)
	require.Truef(t, ok, "controller clientset is %T, want *fake.Clientset", r.controller.clientset)
	return clientset
}

// events returns every event emitted under one reason, so a test can assert on
// how many there are and not only that there was one.
func (r *gatedRestore) events(t *testing.T, reason string) []*corev1.Event {
	t.Helper()
	return eventsForReason(r.clientset(t), reason)
}

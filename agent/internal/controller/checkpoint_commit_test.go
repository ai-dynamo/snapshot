// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	snapshottypes "github.com/ai-dynamo/snapshot/agent/internal/types"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestLostCommitReplyRecoversAfterControllerRestart(t *testing.T) {
	ctx := context.Background()
	content := boundWorkOrder("content", "node-a", "lost-reply")
	pod := makeSourcePod()
	w := makeNodeController(t, &fakeCheckpointer{}, content, pod)
	w.runtime = &fakeRuntime{resolveContainerPID: 7}
	published := false
	probe := func(context.Context, *pagebroker.PublishedArtifact) (*snapshottypes.CheckpointManifest, error) {
		if !published {
			return nil, pagebroker.NewFailureError(pagebroker.Failure_ARTIFACT_NOT_FOUND, "not published")
		}
		return publishedManifest(content), nil
	}
	w.fetchManifestFn = probe
	captures := 0
	w.checkpointFn = func(context.Context, CheckpointParams) (*pagebroker.PublishedArtifact, error) {
		captures++
		published = true
		return nil, fmt.Errorf("checkpoint: %w", &executor.CheckpointCommitError{
			Err: pagebroker.NewFailureError(pagebroker.Failure_TRANSACTION_NOT_FOUND, "broker restarted"),
		})
	}

	require.Error(t, w.reconcileCapture(ctx, content.Name))
	pending := getContent(t, w, content.Name)
	assert.False(t, isContentTerminal(pending))
	condition := meta.FindStatusCondition(pending.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady)
	require.NotNil(t, condition)
	assert.Equal(t, checkpointCommitPendingReason, condition.Reason)

	// A new controller has no in-memory transaction or source pod; recovery must use the stored condition and publication.
	restarted := makeNodeController(t, &fakeCheckpointer{}, pending)
	restarted.fetchManifestFn = probe
	restarted.checkpointFn = w.checkpointFn
	require.NoError(t, restarted.reconcileCapture(ctx, content.Name))
	ready := getContent(t, restarted, content.Name)
	assert.True(t, isContentReady(ready))
	require.NotNil(t, ready.Status.Storage)
	assert.Len(t, ready.Status.Storage.Artifacts, 1)
	assert.Equal(t, 1, captures)
}

func TestUncertainCommitWaitsForPublicationWithoutAnotherDump(t *testing.T) {
	for _, tt := range []struct {
		name     string
		probeErr error
		age      time.Duration
	}{
		{name: "not visible yet", probeErr: pagebroker.NewFailureError(pagebroker.Failure_ARTIFACT_NOT_FOUND, "absent")},
		{name: "broker unavailable", probeErr: errors.New("connection refused")},
		{
			name: "absent after lifetime", probeErr: pagebroker.NewFailureError(pagebroker.Failure_ARTIFACT_NOT_FOUND, "absent"),
			age: 3 * time.Hour,
		},
		{
			name: "unavailable after lifetime", probeErr: errors.New("connection refused"),
			age: 3 * time.Hour,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			content := boundWorkOrder("content", "node-a", "pending")
			content.Status.Conditions = []metav1.Condition{{
				Type: snapshotv1alpha1.PodSnapshotConditionReady, Status: metav1.ConditionUnknown,
				Reason: checkpointCommitPendingReason, LastTransitionTime: metav1.NewTime(time.Now().Add(-tt.age)),
			}}
			w := makeNodeController(t, &fakeCheckpointer{}, content, makeSourcePod())
			w.fetchManifestFn = func(context.Context, *pagebroker.PublishedArtifact) (*snapshottypes.CheckpointManifest, error) {
				return nil, tt.probeErr
			}
			w.checkpointFn = func(context.Context, CheckpointParams) (*pagebroker.PublishedArtifact, error) {
				t.Fatal("an uncertain commit must never trigger another dump")
				return nil, nil
			}
			err := w.reconcileCapture(context.Background(), content.Name)
			require.Error(t, err)
			assert.False(t, isContentFailed(getContent(t, w, content.Name)))
		})
	}
}

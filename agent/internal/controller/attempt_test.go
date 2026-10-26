// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// A retry loop runs attempts directly, so an attempt must report its failure
// rather than record it. Writing Failed is what makes a first attempt the last
// one; keeping that out of the attempt is what lets a caller recover. The
// absence of a client here is part of the assertion: an attempt that reached
// for status would have nothing to write with.
func TestAttemptCheckpointReportsFailureWithoutRecordingIt(t *testing.T) {
	want := errors.New("dump failed")
	w := &NodeController{
		config: &types.AgentConfig{},
		checkpointFn: func(context.Context, CheckpointParams) (*pagebroker.PublishedArtifact, error) {
			return nil, want
		},
	}

	_, err := w.attemptCheckpoint(context.Background(), &snapshotv1alpha1.PodSnapshotContent{}, &corev1.Pod{},
		"main", "cid", 1, false, "uid", "/artifacts/uid")

	if !errors.Is(err, want) {
		t.Errorf("attemptCheckpoint() = %v, want %v", err, want)
	}
}

func TestAttemptCheckpointPassesTheResolvedTarget(t *testing.T) {
	var got CheckpointParams
	w := &NodeController{
		config: &types.AgentConfig{},
		checkpointFn: func(_ context.Context, p CheckpointParams) (*pagebroker.PublishedArtifact, error) {
			got = p
			return nil, nil
		},
	}
	content := &snapshotv1alpha1.PodSnapshotContent{
		Spec: snapshotv1alpha1.PodSnapshotContentSpec{
			Storage: &snapshotv1alpha1.CheckpointStorageBinding{StoreID: "store-a"},
		},
	}

	if _, err := w.attemptCheckpoint(context.Background(), content, &corev1.Pod{},
		"main", "cid", 42, true, "uid", "/artifacts/uid"); err != nil {
		t.Fatalf("attemptCheckpoint() = %v", err)
	}

	if got.ContainerName != "main" || got.ContainerID != "cid" || got.ContainerPID != 42 {
		t.Errorf("container not passed through: %+v", got)
	}
	if got.ContentUID != "uid" || got.HostPath != "/artifacts/uid" {
		t.Errorf("destination not passed through: %+v", got)
	}
	if got.StoreID != "store-a" {
		t.Errorf("store not passed through: %+v", got)
	}
	if !got.CuInterposeRequested {
		t.Error("cuInterposeRequested not passed through")
	}
	if got.StartedAt.IsZero() {
		t.Error("StartedAt not set")
	}
}

// A retry loop applies one budget around its attempts. An attempt that started
// a deadline of its own would reset that budget on every try, so retries could
// outlast it however many times they ran.
func TestAttemptCheckpointInheritsTheCallersBudget(t *testing.T) {
	var dumpDeadline time.Time
	var dumpHasDeadline bool
	w := &NodeController{
		// Far shorter than the caller's budget, so an attempt imposing its own
		// deadline is visible rather than masked by the earlier parent deadline.
		config: &types.AgentConfig{Checkpoint: types.CheckpointSpec{CheckpointTimeoutSeconds: ptr.To(1)}},
		checkpointFn: func(ctx context.Context, _ CheckpointParams) (*pagebroker.PublishedArtifact, error) {
			dumpDeadline, dumpHasDeadline = ctx.Deadline()
			return nil, nil
		},
	}
	budget, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	want, _ := budget.Deadline()

	if _, err := w.attemptCheckpoint(budget, &snapshotv1alpha1.PodSnapshotContent{}, &corev1.Pod{},
		"main", "cid", 1, false, "uid", "/a"); err != nil {
		t.Fatalf("attemptCheckpoint() = %v", err)
	}

	if !dumpHasDeadline {
		t.Fatal("the dump ran without the caller's budget")
	}
	if !dumpDeadline.Equal(want) {
		t.Errorf("dump deadline = %v, want the caller's %v", dumpDeadline, want)
	}
}

func TestRunCheckpointRecordsEitherOutcome(t *testing.T) {
	for _, tc := range []struct {
		name      string
		dumpErr   error
		wantReady bool
	}{
		{"a failed dump becomes terminal", errors.New("dump failed"), false},
		{"a completed dump becomes ready", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := makeWorkOrder("content-a", "node-a", "a")
			w := makeNodeController(t, &fakeCheckpointer{err: tc.dumpErr}, content)

			err := w.runCheckpoint(context.Background(), content, makeSourcePod(),
				"main", "cid", 42, false, string(content.UID), t.TempDir())

			require.NoError(t, err, "a recorded outcome leaves the queue nothing to retry")
			got := getContent(t, w, content.Name)
			require.Equal(t, tc.wantReady, isContentReady(got))
			require.Equal(t, !tc.wantReady, isContentFailed(got))
			if !tc.wantReady {
				assert.Equal(t, "CheckpointFailed",
					meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed).Reason)
			}
		})
	}
}

// The other half of the contract: an outcome that was not recorded must reach
// the queue as an error, or the failure is lost entirely.
func TestRunCheckpointReturnsAnUnrecordedFailure(t *testing.T) {
	content := makeWorkOrder("content-a", "node-a", "a")
	funcs := interceptor.Funcs{
		SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
			return errors.New("apiserver unavailable")
		},
	}
	w := makeNodeControllerWithInterceptor(t, &fakeCheckpointer{err: errors.New("dump failed")}, funcs, content)

	err := w.runCheckpoint(context.Background(), content, makeSourcePod(),
		"main", "cid", 42, false, string(content.UID), t.TempDir())

	require.ErrorContains(t, err, "apiserver unavailable")
	assert.Empty(t, getContent(t, w, content.Name).Status.Conditions)
}

// A budget that runs out during the dump comes back as the attempt's error. The
// attempt records nothing for it — this controller has no client, so an attempt
// reaching for status would have nothing to write with.
func TestAttemptCheckpointReportsAnExhaustedBudget(t *testing.T) {
	w := &NodeController{
		config: &types.AgentConfig{},
		checkpointFn: func(ctx context.Context, _ CheckpointParams) (*pagebroker.PublishedArtifact, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	budget, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, err := w.attemptCheckpoint(budget, &snapshotv1alpha1.PodSnapshotContent{}, &corev1.Pod{},
		"main", "cid", 1, false, "uid", "/a")

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("attemptCheckpoint() = %v, want %v", err, context.DeadlineExceeded)
	}
}

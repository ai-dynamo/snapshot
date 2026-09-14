// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/api/compat"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

func TestRestorePhaseDestinationDecisions(t *testing.T) {
	for _, phase := range []restorePhase{restoreInitial, restoreReplenishing} {
		for _, tc := range []struct {
			name    string
			tracked bool
			current string
		}{
			{"untracked missing", false, ""},
			{"untracked running", false, "new"},
			{"tracked missing", true, ""},
			{"tracked unchanged", true, "old"},
			{"tracked replaced", true, "new"},
		} {
			t.Run(string(phase)+"/"+tc.name, func(t *testing.T) {
				wantSkip := (!tc.tracked && phase == restoreReplenishing) || (tc.tracked && tc.current == "old")
				assert.Equal(t, wantSkip, phase.skipDestination("old", tc.tracked, tc.current))
			})
		}
	}
}

func TestRefusedRestoreClearsIntentForExplicitOverride(t *testing.T) {
	pod := multiRestorePod()
	w := makeTestController(t, pod)
	plan := &restorePlan{artifact: &restoreArtifact{}}
	files := map[string]bool{}
	w.writeControlSentinelFn = func(_ int, name string) error { files[name] = true; return nil }
	w.controlSentinelExistsFn = func(_ int, name string) (bool, error) { return files[name], nil }
	w.removeControlSentinelFn = func(_ int, name string) error { delete(files, name); return nil }
	calls := 0
	w.restoreFn = func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (int, error) {
		calls++
		if !plan.skipCompatCheck {
			return 0, &compat.IncompatibleError{Mismatches: []compat.Mismatch{{Check: "cpu-arch"}}}
		}
		return 4242, nil
	}
	err := w.runRestore(context.Background(), pod, plan, "engine-0", "id", time.Time{})
	var incompatible *compat.IncompatibleError
	require.ErrorAs(t, err, &incompatible)
	assert.Empty(t, files)
	plan.skipCompatCheck = true
	require.NoError(t, w.runRestore(context.Background(), pod, plan, "engine-0", "id", time.Time{}))
	assert.Equal(t, 2, calls)
}

func TestRestoreReleaseFailureRecoversWithoutReplay(t *testing.T) {
	pod := multiRestorePod()
	w := makeTestController(t, pod)
	plan := &restorePlan{artifact: &restoreArtifact{}}
	files := map[string]bool{}
	w.controlSentinelExistsFn = func(_ int, name string) (bool, error) { return files[name], nil }
	w.writeControlSentinelFn = func(_ int, name string) error {
		if name == podcontract.RestoreCompleteFile {
			return errors.New("release write failed")
		}
		files[name] = true
		return nil
	}
	calls := 0
	w.restoreFn = func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (int, error) {
		calls++
		return 4242, nil
	}
	require.ErrorContains(t, w.runRestore(context.Background(), pod, plan, "engine-0", "id", time.Time{}), "release write failed")
	restarted := makeTestController(t, pod)
	restarted.controlSentinelExistsFn = w.controlSentinelExistsFn
	restarted.writeControlSentinelFn = func(_ int, name string) error { files[name] = true; return nil }
	restarted.restoreFn = w.restoreFn
	require.NoError(t, restarted.runRestore(context.Background(), pod, plan, "engine-0", "id", time.Time{}))
	assert.Equal(t, 1, calls)
	assert.True(t, files[podcontract.RestoreCompleteFile])
}

func TestIncarnationEvidenceIsBoundToRestoreIdentity(t *testing.T) {
	pod := multiRestorePod()
	w := makeTestController(t, pod)
	plan := &restorePlan{artifact: &restoreArtifact{ContentUID: "content"}}
	baseline := w.newRestoreOperation(pod, plan, "engine-0", "id", time.Time{}).incarnationSentinel("completed")
	otherPod := pod.DeepCopy()
	otherPod.UID = "other-pod"
	for _, op := range []*restoreOperation{
		w.newRestoreOperation(otherPod, plan, "engine-0", "id", time.Time{}),
		w.newRestoreOperation(pod, &restorePlan{artifact: &restoreArtifact{ContentUID: "other-content"}}, "engine-0", "id", time.Time{}),
		w.newRestoreOperation(pod, plan, "engine-1", "id", time.Time{}),
		w.newRestoreOperation(pod, plan, "engine-0", "other-id", time.Time{}),
	} {
		assert.NotEqual(t, baseline, op.incarnationSentinel("completed"))
	}
}

func TestPendingPreflightPreservesRestorePhase(t *testing.T) {
	for _, reason := range []string{
		podcontract.RestoreReasonInProgress,
		podcontract.RestoreReasonSucceeded,
		podcontract.RestoreReasonReplenishing,
		podcontract.RestoreReasonReplenishmentIncompatible,
	} {
		t.Run(reason, func(t *testing.T) {
			pod := multiRestorePod()
			status := corev1.ConditionFalse
			if reason == podcontract.RestoreReasonSucceeded {
				status = corev1.ConditionTrue
			}
			setPodCondition(&pod.Status, corev1.PodCondition{
				Type: corev1.PodConditionType(podcontract.RestoredCondition), Status: status, Reason: reason,
			})
			w := makeTestController(t, pod)
			require.True(t, w.handleRestorePreflightError(context.Background(), pod, newRestorePendingError("ArtifactPending", "waiting")))
			assert.False(t, hasPodStatusApply(w))
			assert.Equal(t, reason, findRestoredCondition(pod).Reason)
		})
	}
}

func TestReplenishmentStartsWithEntirelyMissingContainerStatus(t *testing.T) {
	pod := multiRestorePod()
	setRestoredContainerIDs(t, pod, map[string]string{"engine-0": "old"})
	setPodCondition(&pod.Status, corev1.PodCondition{
		Type: corev1.PodConditionType(podcontract.RestoredCondition), Status: corev1.ConditionTrue, Reason: podcontract.RestoreReasonSucceeded,
	})
	pod.Status.ContainerStatuses = nil
	w := makeTestController(t, pod)
	assert.True(t, w.hasRestartedRestoreDestination(pod))
	w.runtime = &fakeRuntime{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	plan := &restorePlan{artifact: &restoreArtifact{}, mappings: []podcontract.ContainerMapping{
		{Source: "main", Destination: "engine-0"}, {Source: "main", Destination: "engine-1"},
	}}
	assert.True(t, w.restorePodContainers(ctx, pod, plan, "inference/restore-worker"))
	live, err := w.clientset.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, podcontract.RestoreReasonReplenishing, findRestoredCondition(live).Reason)
	assert.False(t, isRestoreTerminal(live))
}

func TestReplenishmentCompatibilityOverridePreservesUntrackedSibling(t *testing.T) {
	pod := multiRestorePod()
	setRestoredContainerIDs(t, pod, map[string]string{"engine-0": "old"})
	setPodCondition(&pod.Status, corev1.PodCondition{
		Type: corev1.PodConditionType(podcontract.RestoredCondition), Status: corev1.ConditionTrue, Reason: podcontract.RestoreReasonSucceeded,
	})
	w := makeTestController(t, pod)
	require.False(t, w.refuseRestore(context.Background(), pod, &compat.IncompatibleError{
		Mismatches: []compat.Mismatch{{Check: "cpu-arch", Source: "amd64", Target: "arm64"}},
	}))
	live, err := w.clientset.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, podcontract.RestoreReasonReplenishmentIncompatible, findRestoredCondition(live).Reason)
	live.Annotations[podcontract.SkipCompatCheckAnnotation] = "true"
	w = makeTestController(t, live)
	assert.True(t, w.skipRequestedAfterRefusal(live))
	w.restoreFn = func(_ context.Context, _ snapshotruntime.Runtime, _ logr.Logger, req executor.RestoreRequest, _ executor.RestoreMounter) (int, error) {
		assert.Equal(t, "engine-0", req.DestinationContainerName)
		return 4242, nil
	}
	plan := &restorePlan{artifact: &restoreArtifact{}, skipCompatCheck: true, mappings: []podcontract.ContainerMapping{
		{Source: "main", Destination: "engine-0"}, {Source: "main", Destination: "engine-1"},
	}}
	assert.False(t, w.restorePodContainers(context.Background(), live, plan, "inference/restore-worker"))
	assert.Empty(t, liveRestoredContainerID(t, w, live, "engine-1"))
}

func TestRestoreRecoveryInterruptionBoundaries(t *testing.T) {
	for _, phase := range []restorePhase{restoreInitial, restoreReplenishing} {
		for _, tc := range []struct {
			name       string
			started    bool
			completed  bool
			wantReplay bool
			wantError  bool
		}{
			{"before intent", false, false, true, false},
			{"after intent before completion", true, false, false, true},
			{"after completion before release or status", true, true, false, false},
		} {
			t.Run(string(phase)+"/"+tc.name, func(t *testing.T) {
				pod := multiRestorePod()
				setPodCondition(&pod.Status, corev1.PodCondition{
					Type: corev1.PodConditionType(podcontract.RestoredCondition), Status: corev1.ConditionFalse, Reason: string(phase),
				})
				w := makeTestController(t, pod)
				plan := &restorePlan{artifact: &restoreArtifact{ContentUID: "content"}}
				op := w.newRestoreOperation(pod, plan, "engine-0", "new-id", time.Time{})
				files := map[string]bool{
					// A previous incarnation's release and completion must not
					// count as completion of this incarnation.
					podcontract.RestoreCompleteFile: true,
					w.newRestoreOperation(pod, plan, "engine-0", "old-id", time.Time{}).incarnationSentinel("completed"): true,
					op.incarnationSentinel("started"):   tc.started,
					op.incarnationSentinel("completed"): tc.completed,
				}
				w.controlSentinelExistsFn = func(_ int, name string) (bool, error) { return files[name], nil }
				released := false
				w.writeControlSentinelFn = func(_ int, name string) error {
					files[name] = true
					if name == podcontract.RestoreCompleteFile {
						require.True(t, files[op.incarnationSentinel("completed")])
						released = true
					}
					return nil
				}
				calls := 0
				w.restoreFn = func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (int, error) {
					require.True(t, files[op.incarnationSentinel("started")])
					calls++
					return 4242, nil
				}
				err := w.runRestore(context.Background(), pod, plan, "engine-0", "new-id", time.Time{})
				if tc.wantError {
					require.ErrorContains(t, err, "uncertain")
					assert.False(t, released)
				} else {
					require.NoError(t, err)
					assert.True(t, released)
				}
				assert.Equal(t, tc.wantReplay, calls == 1)
			})
		}
	}
}

func TestRestoreEvidenceWriteFailures(t *testing.T) {
	for _, stage := range []string{"started", "completed"} {
		t.Run(stage, func(t *testing.T) {
			pod := multiRestorePod()
			w := makeTestController(t, pod)
			plan := &restorePlan{artifact: &restoreArtifact{}}
			op := w.newRestoreOperation(pod, plan, "engine-0", "id", time.Time{})
			w.writeControlSentinelFn = func(_ int, name string) error {
				assert.NotEqual(t, podcontract.RestoreCompleteFile, name, "must not release an unrecorded restore")
				if name == op.incarnationSentinel(stage) {
					return errors.New("disk write failed")
				}
				return nil
			}
			calls := 0
			w.restoreFn = func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (int, error) {
				calls++
				return 4242, nil
			}
			require.ErrorContains(t, w.runRestore(context.Background(), pod, plan, "engine-0", "id", time.Time{}), "disk write failed")
			assert.Equal(t, stage == "completed", calls == 1)
		})
	}
}

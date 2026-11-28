// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/podcontract"
	v1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

func TestRestoreVerificationGateBlocksConsumersAndReleasesAfterSuccess(t *testing.T) {
	consumer := restorePod(map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"})
	s, c := readySnapshotObjects()
	now := time.Now().UTC().Truncate(time.Second)
	s.Spec.RestoreVerification = &v1.RestoreVerificationPolicy{
		PodRef:                   v1.RestoreVerificationPodReference{Name: "canary", UID: "canary-uid"},
		ObservationWindowSeconds: 10, TimeoutSeconds: 60,
	}
	s.Status.Phase = v1.PodSnapshotPhaseRestoreVerifying
	s.Status.RestoreVerifyInfo = &v1.RestoreVerificationInfo{
		ContentUID: c.UID, PodRef: s.Spec.RestoreVerification.PodRef,
		PolicyHash: v1.RestoreVerificationPolicyHash(s.Spec.RestoreVerification),
		StartedAt:  metav1.NewTime(now), DeadlineAt: metav1.NewTime(now.Add(time.Minute)),
		Outcome:    v1.RestoreVerificationInProgress,
		Containers: []v1.RestoreVerificationContainer{{Name: "main", ContainerID: "containerd://canary-1"}},
	}
	w := makeTestController(t, consumer, s, c)
	w.nowFn = func() time.Time { return now }
	path, err := nsmount.ResolveArtifactPath(w.config.Storage.BasePath, string(c.UID), "main")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(path, 0o700))
	require.NoError(t, types.WriteManifest(path, &types.CheckpointManifest{Artifact: types.ArtifactManifest{ContentUID: string(c.UID), ContainerName: "main"}}))
	var restored []string
	w.restoreFn = func(_ context.Context, _ snapshotruntime.Runtime, _ logr.Logger, req executor.RestoreRequest, _ executor.RestoreMounter) (executor.RestoreResult, error) {
		restored = append(restored, req.PodName)
		return executor.RestoreResult{PlaceholderHostPID: 4242, RestoredPID: 1}, nil
	}
	// The ready artifact exists, but a regular consumer cannot start restore.
	require.True(t, w.reconcileRestorePod(context.Background(), consumer))
	require.Empty(t, restored)
	require.Contains(t, string(lastPodStatusApply(t, w).GetPatch()), "VerificationPending")
	// The recorded canary is the sole exception, with the pinned execution.
	canary := consumer.DeepCopy()
	canary.Name, canary.UID = "canary", "canary-uid"
	canary.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "main", ContainerID: "containerd://canary-1", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	plan, err := w.preflightRestore(context.Background(), canary)
	require.NoError(t, err)
	require.Equal(t, string(c.UID), plan.artifact.ContentUID)
	// Approval on this exact artifact releases the same waiting consumer.
	live := &v1.PodSnapshot{}
	require.NoError(t, w.client.Get(context.Background(), client.ObjectKeyFromObject(s), live))
	live.Status.Phase = v1.PodSnapshotPhaseCompleted
	live.Status.RestoreVerifyInfo.Outcome = v1.RestoreVerificationSucceeded
	done := metav1.NewTime(now.Add(10 * time.Second))
	live.Status.RestoreVerifyInfo.CompletedAt = &done
	require.NoError(t, w.client.Update(context.Background(), live))
	require.False(t, w.reconcileRestorePod(context.Background(), consumer))
	require.Equal(t, []string{consumer.Name}, restored)
}

func TestRestoreVerificationFailureNotifiesWaitingProxy(t *testing.T) {
	for _, timedOut := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed", true: "timeout"}[timedOut], func(t *testing.T) {
			p := restorePod(map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"})
			s, c := readySnapshotObjects()
			now := time.Now()
			s.Spec.RestoreVerification = &v1.RestoreVerificationPolicy{PodRef: v1.RestoreVerificationPodReference{Name: "canary", UID: "canary-uid"}, TimeoutSeconds: 60}
			s.Status.Phase = v1.PodSnapshotPhaseVerificationFailed
			s.Status.RestoreVerifyInfo = &v1.RestoreVerificationInfo{
				ContentUID: c.UID, PolicyHash: v1.RestoreVerificationPolicyHash(s.Spec.RestoreVerification), PodRef: s.Spec.RestoreVerification.PodRef,
				Outcome: v1.RestoreVerificationFailed, Reason: "VerificationRestoreFailed", DeadlineAt: metav1.NewTime(now.Add(-time.Second)),
			}
			if timedOut {
				s.Status.Phase = v1.PodSnapshotPhaseRestoreVerifying
				s.Status.RestoreVerifyInfo.Outcome = v1.RestoreVerificationInProgress
			}
			w := makeTestController(t, p, s, c)
			var notifications []string
			w.writePodControlSentinelFn = func(uid, dest, name string, data []byte) error {
				notifications = append(notifications, uid+"/"+dest+"/"+name+":"+string(data))
				return nil
			}
			w.restoreFn = func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (executor.RestoreResult, error) {
				t.Fatal("a failed snapshot must not enter restore")
				return executor.RestoreResult{}, nil
			}
			require.False(t, w.reconcileRestorePod(context.Background(), p))
			require.Equal(t, []string{string(p.UID) + "/main/restore-failed:failed\n"}, notifications)
			require.Contains(t, string(lastPodStatusApply(t, w).GetPatch()), `"reason":"RestoreFailed"`)
		})
	}
}

func TestRestoreVerificationRecordMismatchIsTerminal(t *testing.T) {
	s, c := readySnapshotObjects()
	s.Spec.RestoreVerification = &v1.RestoreVerificationPolicy{PodRef: v1.RestoreVerificationPodReference{Name: "canary", UID: "canary-uid"}, TimeoutSeconds: 60}
	s.Status.Phase = v1.PodSnapshotPhaseCompleted
	s.Status.RestoreVerifyInfo = &v1.RestoreVerificationInfo{ContentUID: "old-content", Outcome: v1.RestoreVerificationSucceeded}
	err := validateRestoreVerification(s, c, restorePod(nil), time.Now())
	var failed *restoreVerificationError
	require.True(t, errors.As(err, &failed))
	require.Contains(t, err.Error(), "does not match")
}

func TestRestoreVerificationInvalidMappingDoesNotRetryForever(t *testing.T) {
	p := restorePod(map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a", podcontract.RestoreContainerMapAnnotation: "not-json"})
	s, c := readySnapshotObjects()
	s.Spec.RestoreVerification = &v1.RestoreVerificationPolicy{PodRef: v1.RestoreVerificationPodReference{Name: "canary", UID: "canary-uid"}, TimeoutSeconds: 60}
	s.Status.Phase = v1.PodSnapshotPhaseVerificationFailed
	w := makeTestController(t, p, s, c)
	require.False(t, w.reconcileRestorePod(context.Background(), p))
	require.Contains(t, string(lastPodStatusApply(t, w).GetPatch()), "invalid restore destinations")
	require.True(t, w.restoreHandled(p))
}

func TestRestoreVerificationCaptureFailureNotifiesProxy(t *testing.T) {
	p := restorePod(map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"})
	s, c := readySnapshotObjects()
	s.Spec.RestoreVerification = &v1.RestoreVerificationPolicy{PodRef: v1.RestoreVerificationPodReference{Name: "canary", UID: "canary-uid"}, TimeoutSeconds: 60}
	meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{Type: v1.PodSnapshotConditionFailed, Status: metav1.ConditionTrue, Reason: "CaptureFailed"})
	w := makeTestController(t, p, s, c)
	notified := false
	w.writePodControlSentinelFn = func(_, _, name string, _ []byte) error {
		notified = name == podcontract.RestoreFailedFile
		return nil
	}
	require.False(t, w.reconcileRestorePod(context.Background(), p))
	require.True(t, notified)
	require.True(t, w.restoreHandled(p))
}

func TestRestoreVerificationRetriesProxyNotificationBeforeTerminalStatus(t *testing.T) {
	p := restorePod(map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"})
	s, c := readySnapshotObjects()
	s.Spec.RestoreVerification = &v1.RestoreVerificationPolicy{PodRef: v1.RestoreVerificationPodReference{Name: "canary", UID: "canary-uid"}, TimeoutSeconds: 60}
	s.Status.Phase = v1.PodSnapshotPhaseVerificationFailed
	w := makeTestController(t, p, s, c)
	attempts := 0
	w.writePodControlSentinelFn = func(_, _, _ string, _ []byte) error {
		attempts++
		if attempts == 1 {
			return os.ErrNotExist // kubelet has not mounted the control volume yet
		}
		return nil
	}
	w.restoreFn = func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (executor.RestoreResult, error) {
		t.Fatal("failed verification must never start restore")
		return executor.RestoreResult{}, nil
	}
	require.True(t, w.reconcileRestorePod(context.Background(), p))
	require.False(t, w.restoreHandled(p), "must retain work while proxy notification is pending")
	require.False(t, w.reconcileRestorePod(context.Background(), p))
	require.True(t, w.restoreHandled(p))
	require.Equal(t, 2, attempts)
}

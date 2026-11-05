//go:build integration

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// TestPodGroupRestoreRefusalFitsInStatus refuses the largest restore with a
// refusal message of 32 KiB and records the status through the reconciler's
// own patch, against a real API server. Every member fails, so copying the
// refusal into each of them would put 8 MiB of messages in one object.
func TestPodGroupRestoreRefusalFitsInStatus(t *testing.T) {
	cl := startPodGroupEnvtest(t)
	restore := &snapshotv1alpha1.PodGroupRestore{
		ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("r", 253), Namespace: "default"},
		Spec: snapshotv1alpha1.PodGroupRestoreSpec{
			SnapshotRef:     snapshotv1alpha1.ObjectIdentity{Name: strings.Repeat("g", 63), UID: "snapshot-uid"},
			DeadlineSeconds: 600,
			Targets:         longestPodGroupRestoreTargets(256),
		},
	}
	require.NoError(t, cl.Create(t.Context(), restore))

	var patchSize int
	patching := interceptor.NewClient(cl, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object,
			patch client.Patch, opts ...client.SubResourcePatchOption,
		) error {
			data, err := patch.Data(obj)
			if err != nil {
				return err
			}
			patchSize = len(data)
			return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
		},
	})
	r := &PodGroupRestoreReconciler{Client: patching, Recorder: record.NewFakeRecorder(10)}

	refusal := strings.Repeat("x", 32<<10)
	now := time.Now()
	observed := podGroupRestoreObservation{
		members:   make([]podGroupRestoreMemberObservation, len(restore.Spec.Targets)),
		admission: podGroupRefusal(refusal),
		deadline:  now.Add(time.Hour),
	}
	desired := derivePodGroupRestoreStatus(&restore.Spec, observed, restore.Status, now)
	require.NoError(t, r.recordStatus(t.Context(), restore, desired))
	t.Logf("status patch: %d bytes", patchSize)
	assert.Less(t, patchSize, 1<<20, "status patch bytes")

	stored := &snapshotv1alpha1.PodGroupRestore{}
	require.NoError(t, cl.Get(t.Context(), client.ObjectKeyFromObject(restore), stored))
	failed := meta.FindStatusCondition(stored.Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, refusal, failed.Message, "the restore's Failed condition keeps the refusal")
	require.Len(t, stored.Status.Members, len(restore.Spec.Targets))
	for _, m := range stored.Status.Members {
		assert.Equal(t, snapshotv1alpha1.RestorePhaseFailed, m.Phase, m.ID)
		assert.Equal(t, snapshotv1alpha1.PodGroupReasonRefused, m.Reason, m.ID)
		assert.Equal(t, podGroupRestoreRefusedMemberMessage, m.Message, m.ID)
	}
}

// longestPodGroupRestoreTargets returns n targets whose IDs, Pod names, UIDs
// and container mappings are at their maximum lengths.
func longestPodGroupRestoreTargets(n int) []snapshotv1alpha1.PodGroupRestoreTarget {
	targets := make([]snapshotv1alpha1.PodGroupRestoreTarget, n)
	for i := range targets {
		id, pod, uid := fmt.Sprintf("target-%d", i), fmt.Sprintf("pod-%d", i), fmt.Sprintf("pod-uid-%d", i)
		targets[i] = snapshotv1alpha1.PodGroupRestoreTarget{
			ID: id + strings.Repeat("x", 63-len(id)),
			PodRef: snapshotv1alpha1.TargetPodReference{
				Name: pod + strings.Repeat("x", 253-len(pod)),
				UID:  types.UID(uid + strings.Repeat("x", 128-len(uid))),
			},
			ContainerMappings: []snapshotv1alpha1.ContainerMapping{{
				Source: strings.Repeat("s", 63), Destination: strings.Repeat("d", 63),
			}},
		}
	}
	return targets
}

// TestPodGroupRestoreReadsOnlyItsOwnReport reuses a restore Pod that still
// carries an earlier attempt's report, against a real API server, with every
// report written as the contract requires (writeReport). The Pod is pinned and
// its member stays Admitted; a report under this restore's UID then fails the
// member, and its annotation and conditions land in one write.
func TestPodGroupRestoreReadsOnlyItsOwnReport(t *testing.T) {
	cl := startPodGroupEnvtest(t)
	ctx := t.Context()
	const ns = "default"
	require.NoError(t, cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: pgAgentNamespace}}))

	snapshot := &snapshotv1alpha1.PodGroupSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "group", Namespace: ns},
		Spec: snapshotv1alpha1.PodGroupSnapshotSpec{DeadlineSeconds: 600, Members: []snapshotv1alpha1.PodGroupSnapshotMember{{
			ID: "a", Source: snapshotv1alpha1.PodSnapshotSource{PodRef: snapshotv1alpha1.PodReference{
				Name: "source-a", UID: "source-uid-a", Containers: []string{"main"},
			}},
		}}},
	}
	require.NoError(t, cl.Create(ctx, snapshot))
	snapshot.Status.Conditions = []metav1.Condition{{Type: snapshotv1alpha1.PodGroupConditionReady, Status: metav1.ConditionTrue,
		Reason: "Checkpointed", Message: "all 1 members are checkpointed", LastTransitionTime: metav1.Now()}}
	require.NoError(t, cl.Status().Update(ctx, snapshot))

	restore := &snapshotv1alpha1.PodGroupRestore{
		ObjectMeta: metav1.ObjectMeta{Name: "restore", Namespace: ns},
		Spec: snapshotv1alpha1.PodGroupRestoreSpec{
			SnapshotRef:     snapshotv1alpha1.ObjectIdentity{Name: "group", UID: snapshot.UID},
			DeadlineSeconds: 600,
			Targets:         []snapshotv1alpha1.PodGroupRestoreTarget{{ID: "a", PodRef: snapshotv1alpha1.TargetPodReference{Name: "target-a"}}},
		},
	}
	require.NoError(t, cl.Create(ctx, restore))

	agent := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-node-1", Namespace: pgAgentNamespace, Labels: pgAgentLabels,
			Annotations: map[string]string{snapshotv1alpha1.PodGroupProtocolsAnnotation: "v1"}},
		Spec: corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{{Name: "agent", Image: "agent:latest"}}},
	}
	require.NoError(t, cl.Create(ctx, agent))
	agent.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	require.NoError(t, cl.Status().Update(ctx, agent))

	target := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "target-a", Namespace: ns, Annotations: map[string]string{
			podcontract.RestoreGroupAnnotation:    restore.Name,
			podcontract.RestoreGroupUIDAnnotation: string(restore.UID),
		}},
		Spec: corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{{Name: "main", Image: "workload:latest"}}},
	}
	require.NoError(t, cl.Create(ctx, target))
	target.Status = corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.1.0.5", ContainerStatuses: []corev1.ContainerStatus{{
		Name: "main", Image: "workload:latest", ImageID: "workload@sha256:0", ContainerID: "containerd://a", Ready: true,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
	}}}
	require.NoError(t, cl.Status().Update(ctx, target))
	writeReport(t, cl, target, "earlier-uid",
		corev1.PodCondition{Type: podcontract.RestoredCondition, Status: corev1.ConditionFalse, Reason: podcontract.RestoreReasonFailed},
		corev1.PodCondition{Type: podcontract.GroupRestoreFailedCondition, Status: corev1.ConditionTrue, Reason: snapshotv1alpha1.PodGroupReasonRefused})

	r := &PodGroupRestoreReconciler{
		Client: cl, NonCacheReadClient: cl, Recorder: record.NewFakeRecorder(10),
		AgentNamespace: pgAgentNamespace, AgentSelector: labels.SelectorFromSet(pgAgentLabels),
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(restore)}
	_, err := r.Reconcile(ctx, request) // adds the finalizer
	require.NoError(t, err)
	reconcile := func() *snapshotv1alpha1.PodGroupRestoreMemberStatus {
		t.Helper()
		_, err := r.Reconcile(ctx, request)
		require.NoError(t, err)
		require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(restore), restore))
		require.Len(t, restore.Status.Members, 1)
		return &restore.Status.Members[0]
	}
	m := reconcile()
	require.NotNil(t, m.Pod, "the reused Pod is pinned")
	assert.Equal(t, target.UID, m.Pod.UID)
	assert.Equal(t, snapshotv1alpha1.RestorePhaseAdmitted, m.Phase, "the earlier attempt's report is not read")
	assert.Nil(t, meta.FindStatusCondition(restore.Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed))

	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(target), target))
	before := target.DeepCopy()
	writeReport(t, cl, target, string(restore.UID),
		corev1.PodCondition{Type: podcontract.RestoreStartedCondition, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now()},
		corev1.PodCondition{Type: podcontract.RestoredCondition, Status: corev1.ConditionFalse, Reason: podcontract.RestoreReasonFailed},
		corev1.PodCondition{Type: podcontract.GroupRestoreFailedCondition, Status: corev1.ConditionTrue,
			Reason: snapshotv1alpha1.PodGroupReasonPodIPChanged, Message: "the Pod's IP changed"})

	stored := &corev1.Pod{}
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(target), stored))
	assert.Equal(t, target.ResourceVersion, stored.ResourceVersion, "nothing wrote the Pod since the report")
	assert.Equal(t, string(restore.UID), stored.Annotations[podcontract.RestoreGroupReportUIDAnnotation],
		"the status write persisted the annotation")
	assert.Len(t, stored.Status.Conditions, 3, "and the conditions, in the same write")

	t.Run("a report written against a stale resourceVersion is refused", func(t *testing.T) {
		stale := before.DeepCopy()
		stale.Annotations[podcontract.RestoreGroupReportUIDAnnotation] = "stale-uid"
		err := cl.Status().Patch(ctx, stale, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
		assert.True(t, apierrors.IsConflict(err), "want Conflict, got %v", err)
	})

	m = reconcile()
	assert.Equal(t, snapshotv1alpha1.RestorePhaseFailed, m.Phase)
	assert.Equal(t, snapshotv1alpha1.PodGroupReasonPodIPChanged, m.Reason, "the reason comes from this attempt's GroupRestoreFailed")
	assert.NotNil(t, m.StartedAt)
}

// writeReport writes a report on pod as the contract requires: the report UID
// annotation and the conditions in one pods/status write, conditional on the
// resourceVersion pod holds. pod then holds the written Pod. It is the test's
// writer; the agent's own comes with the agent's group restore.
func writeReport(t *testing.T, cl client.Client, pod *corev1.Pod, reportUID string, conditions ...corev1.PodCondition) {
	t.Helper()
	original := pod.DeepCopy()
	pod.Annotations[podcontract.RestoreGroupReportUIDAnnotation] = reportUID
	pod.Status.Conditions = conditions
	require.NoError(t, cl.Status().Patch(t.Context(), pod, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})))
	require.Equal(t, reportUID, pod.Annotations[podcontract.RestoreGroupReportUIDAnnotation], "the API server kept the annotation")
}

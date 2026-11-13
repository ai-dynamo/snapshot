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

// TestPodGroupRestoreClaimsAndPins runs a restore of two gated targets against
// a real API server, which refuses a gated Pod with a node, so the Pods are
// created without one. The claim writes both annotations, removes Snapshot's
// gate, keeps a gang scheduler's, and changes nothing else. Once the Pods are
// bound to a node through pods/binding and run, every member is pinned. A
// report written as the contract requires, a fenced write of the Pod's
// status, then fails its member.
func TestPodGroupRestoreClaimsAndPins(t *testing.T) {
	cl := startPodGroupEnvtest(t)
	ctx := t.Context()
	const ns = "default"
	require.NoError(t, cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: pgAgentNamespace}}))

	snapshot := &snapshotv1alpha1.PodGroupSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "group", Namespace: ns},
		Spec:       snapshotv1alpha1.PodGroupSnapshotSpec{DeadlineSeconds: 600},
	}
	for _, id := range []string{"a", "b"} {
		snapshot.Spec.Members = append(snapshot.Spec.Members, snapshotv1alpha1.PodGroupSnapshotMember{
			ID: id, Source: snapshotv1alpha1.PodSnapshotSource{PodRef: snapshotv1alpha1.PodReference{
				Name: "source-" + id, UID: types.UID("source-uid-" + id), Containers: []string{"main"},
			}},
		})
	}
	require.NoError(t, cl.Create(ctx, snapshot))
	snapshot.Status.Conditions = []metav1.Condition{{Type: snapshotv1alpha1.PodGroupConditionReady, Status: metav1.ConditionTrue,
		Reason: "Checkpointed", Message: "all 2 members are checkpointed", LastTransitionTime: metav1.Now()}}
	require.NoError(t, cl.Status().Update(ctx, snapshot))

	restore := &snapshotv1alpha1.PodGroupRestore{
		ObjectMeta: metav1.ObjectMeta{Name: "restore", Namespace: ns},
		Spec: snapshotv1alpha1.PodGroupRestoreSpec{
			SnapshotRef:     snapshotv1alpha1.ObjectIdentity{Name: "group", UID: snapshot.UID},
			DeadlineSeconds: 600,
			Targets: []snapshotv1alpha1.PodGroupRestoreTarget{
				{ID: "a", PodRef: snapshotv1alpha1.TargetPodReference{Name: "target-a"}},
				{ID: "b", PodRef: snapshotv1alpha1.TargetPodReference{Name: "target-b"},
					ContainerMappings: []snapshotv1alpha1.ContainerMapping{{Source: "main", Destination: "server"}}},
			},
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

	gang := corev1.PodSchedulingGate{Name: "example.com/gang"}
	created := map[string]*corev1.Pod{}
	for id, destination := range map[string]string{"a": "main", "b": "server"} {
		owned := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "target-" + id, Namespace: ns, Labels: map[string]string{"app": "workload"}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: destination, Image: "workload:latest"}}},
		}
		if id == "a" {
			owned.Spec.SchedulingGates = []corev1.PodSchedulingGate{gang}
		}
		target, err := podcontract.BuildGroupTarget(owned, podcontract.GroupTargetRequest{DestinationContainer: destination},
			podcontract.Options{})
		require.NoError(t, err)
		require.NoError(t, cl.Create(ctx, target))
		created[id] = target
	}

	r := &PodGroupRestoreReconciler{
		Client: cl, NonCacheReadClient: cl, Recorder: record.NewFakeRecorder(10),
		AgentNamespace: pgAgentNamespace, AgentSelector: labels.SelectorFromSet(pgAgentLabels),
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(restore)}
	reconcile := func() *snapshotv1alpha1.PodGroupRestore {
		t.Helper()
		_, err := r.Reconcile(ctx, request)
		require.NoError(t, err)
		require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(restore), restore))
		return restore
	}
	admitted := func() *metav1.Condition {
		t.Helper()
		condition := meta.FindStatusCondition(restore.Status.Conditions, snapshotv1alpha1.PodGroupConditionAdmitted)
		require.NotNil(t, condition)
		return condition
	}
	pod := func(id string) *corev1.Pod {
		t.Helper()
		got := &corev1.Pod{}
		require.NoError(t, cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: "target-" + id}, got))
		return got
	}

	reconcile() // adds the finalizer
	reconcile() // claims
	for id, mapping := range map[string]string{"a": "main=main", "b": "main=server"} {
		got := pod(id)
		assert.Equal(t, string(restore.UID), got.Annotations[podcontract.RestoreGroupUIDAnnotation], id)
		assert.Equal(t, mapping, got.Annotations[podcontract.RestoreContainerMapAnnotation], id)
		assert.False(t, podcontract.HasRestoreGroupGate(got), "%s: Snapshot's gate is gone", id)
		claim, bound, err := podcontract.RestoreGroupClaimOf(got.Annotations)
		require.NoError(t, err, id)
		require.True(t, bound, id)
		want, err := podcontract.ClaimGroupTarget(created[id], claim)
		require.NoError(t, err, id)
		assert.Equal(t, want.Spec, got.Spec, "%s: the claim changed nothing else in the spec", id)
		assert.Equal(t, want.Labels, got.Labels, id)
		assert.Equal(t, want.Annotations, got.Annotations, id)
	}
	assert.Equal(t, []corev1.PodSchedulingGate{gang}, pod("a").Spec.SchedulingGates, "the gang's gate stays")

	reconcile()
	assert.Equal(t, snapshotv1alpha1.PodGroupReasonTargetPodUnscheduled, admitted().Reason)

	// The gang scheduler lets its Pods go, and the scheduler binds both.
	a := pod("a")
	a.Spec.SchedulingGates = nil
	require.NoError(t, cl.Update(ctx, a))
	for _, id := range []string{"a", "b"} {
		require.NoError(t, cl.SubResource("binding").Create(ctx, pod(id), &corev1.Binding{
			ObjectMeta: metav1.ObjectMeta{Name: "target-" + id},
			Target:     corev1.ObjectReference{Kind: "Node", Name: "node-1"},
		}))
		assert.Equal(t, "node-1", pod(id).Spec.NodeName, id)
	}
	reconcile()
	assert.Equal(t, snapshotv1alpha1.PodGroupReasonTargetPodPending, admitted().Reason)
	assert.Empty(t, restore.Status.Members[0].NodeName, "nothing is pinned")

	// The kubelet starts both.
	for i, id := range []string{"a", "b"} {
		running := pod(id)
		running.Status = corev1.PodStatus{Phase: corev1.PodRunning, PodIP: fmt.Sprintf("10.1.0.%d", i+5),
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: running.Spec.Containers[0].Name, Image: "workload:latest", ImageID: "workload@sha256:0",
				ContainerID: "containerd://" + id, Ready: false,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
			}}}
		require.NoError(t, cl.Status().Update(ctx, running))
	}
	reconcile()
	assert.Equal(t, snapshotv1alpha1.PodGroupReasonAdmitting, admitted().Reason)
	for i, want := range []struct{ id, container, ip string }{{"a", "main", "10.1.0.5"}, {"b", "server", "10.1.0.6"}} {
		m := restore.Status.Members[i]
		assert.Equal(t, snapshotv1alpha1.RestorePhaseAdmitted, m.Phase, want.id)
		assert.Equal(t, &snapshotv1alpha1.ObjectIdentity{Name: "target-" + want.id, UID: pod(want.id).UID}, m.Pod, want.id)
		assert.Equal(t, "node-1", m.NodeName, want.id)
		assert.Equal(t, []snapshotv1alpha1.PodGroupRestoreContainerStatus{{Name: want.container, ContainerID: "containerd://" + want.id}},
			m.Containers, want.id)
		assert.Equal(t, want.ip, m.TargetPodIP, want.id)
	}

	target := pod("a")
	before := target.DeepCopy()
	writeReport(t, cl, target,
		corev1.PodCondition{Type: podcontract.RestoreStartedCondition, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now()},
		corev1.PodCondition{Type: podcontract.RestoredCondition, Status: corev1.ConditionFalse, Reason: podcontract.RestoreReasonFailed},
		corev1.PodCondition{Type: podcontract.GroupRestoreFailedCondition, Status: corev1.ConditionTrue,
			Reason: snapshotv1alpha1.PodGroupReasonPodIPChanged, Message: "the Pod's IP changed"})
	assert.Equal(t, before.Annotations, target.Annotations, "a status write keeps the binding")

	t.Run("a report written against a stale resourceVersion is refused", func(t *testing.T) {
		stale := before.DeepCopy()
		stale.Status.Conditions = append(stale.Status.Conditions, corev1.PodCondition{
			Type: podcontract.RestoredCondition, Status: corev1.ConditionTrue, Reason: podcontract.RestoreReasonSucceeded})
		err := cl.Status().Patch(ctx, stale, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
		assert.True(t, apierrors.IsConflict(err), "want Conflict, got %v", err)
	})

	m := reconcile().Status.Members[0]
	assert.Equal(t, snapshotv1alpha1.RestorePhaseFailed, m.Phase)
	assert.Equal(t, snapshotv1alpha1.PodGroupReasonPodIPChanged, m.Reason, "the reason comes from its GroupRestoreFailed")
	assert.NotNil(t, m.StartedAt)
}

// writeReport writes a report on pod as the contract requires: the conditions
// in one write of the Pod's status, conditional on the resourceVersion pod
// holds. pod then holds the written Pod. It is the test's writer; the agent's
// own comes with the agent's group restore.
func writeReport(t *testing.T, cl client.Client, pod *corev1.Pod, conditions ...corev1.PodCondition) {
	t.Helper()
	original := pod.DeepCopy()
	pod.Status.Conditions = conditions
	require.NoError(t, cl.Status().Patch(t.Context(), pod, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})))
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
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
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

const (
	pgNamespace      = "inference"
	pgAgentNamespace = "snapshot-system"
)

var pgAgentLabels = labels.Set{"app.kubernetes.io/component": "snapshot-agent", "app.kubernetes.io/instance": "snapshot"}

// makePodGroupReconciler builds a reconciler on a fake client that, like the
// API server, gives every created object a UID.
func makePodGroupReconciler(t *testing.T, objs ...client.Object) (*PodGroupSnapshotReconciler, *record.FakeRecorder) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(snapshotReconcilerScheme()).WithObjects(objs...).
		WithStatusSubresource(&snapshotv1alpha1.PodGroupSnapshot{}, &snapshotv1alpha1.PodSnapshot{}, &snapshotv1alpha1.PodSnapshotContent{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetUID() == "" {
					obj.SetUID(types.UID(obj.GetName() + "-uid"))
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()
	recorder := record.NewFakeRecorder(100)
	return &PodGroupSnapshotReconciler{
		Client:             c,
		NonCacheReadClient: c,
		Recorder:           recorder,
		AgentNamespace:     pgAgentNamespace,
		AgentSelector:      labels.SelectorFromSet(pgAgentLabels),
	}, recorder
}

// pgGroup is a PodGroupSnapshot named "group" whose members a, b, ... name the
// source Pods pod-a, pod-b, ...; it already holds its finalizer.
func pgGroup(ids ...string) *snapshotv1alpha1.PodGroupSnapshot {
	return &snapshotv1alpha1.PodGroupSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "group",
			Namespace:         pgNamespace,
			UID:               "group-uid",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Minute).Truncate(time.Second)),
			Finalizers:        []string{snapshotv1alpha1.PodGroupInUseFinalizer},
		},
		Spec: *pgSpec(ids...),
	}
}

func pgSourcePod(id, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-" + id, Namespace: pgNamespace, UID: types.UID("pod-uid-" + id)},
		Spec:       corev1.PodSpec{NodeName: node},
	}
}

// pgAgent is a node agent Pod on node. protocols nil omits the annotation.
func pgAgent(node string, ready bool, protocols *string) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	agent := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-" + node, Namespace: pgAgentNamespace, Labels: pgAgentLabels},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}},
	}
	if protocols != nil {
		agent.Annotations = map[string]string{snapshotv1alpha1.PodGroupProtocolsAnnotation: *protocols}
	}
	return agent
}

// pgReadyCluster returns source Pods for ids on node-1, with a group-capable agent there.
func pgReadyCluster(ids ...string) []client.Object {
	objs := []client.Object{pgAgent("node-1", true, ptr.To("v1"))}
	for _, id := range ids {
		objs = append(objs, pgSourcePod(id, "node-1"))
	}
	return objs
}

func reconcileGroup(t *testing.T, r *PodGroupSnapshotReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Namespace: pgNamespace, Name: "group"}})
	require.NoError(t, err)
	return res
}

func getGroup(t *testing.T, r *PodGroupSnapshotReconciler) *snapshotv1alpha1.PodGroupSnapshot {
	t.Helper()
	group := &snapshotv1alpha1.PodGroupSnapshot{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: pgNamespace, Name: "group"}, group))
	return group
}

func listMemberPodSnapshots(t *testing.T, r *PodGroupSnapshotReconciler) []snapshotv1alpha1.PodSnapshot {
	t.Helper()
	var snaps snapshotv1alpha1.PodSnapshotList
	require.NoError(t, r.List(context.Background(), &snaps, client.InNamespace(pgNamespace)))
	return snaps.Items
}

func groupCondition(t *testing.T, group *snapshotv1alpha1.PodGroupSnapshot, condType string) *metav1.Condition {
	t.Helper()
	return meta.FindStatusCondition(group.Status.Conditions, condType)
}

// drainEvents returns the events recorded so far.
func drainEvents(recorder *record.FakeRecorder) []string {
	var events []string
	for {
		select {
		case e := <-recorder.Events:
			events = append(events, e)
		default:
			return events
		}
	}
}

// pgReport plays the agent and the PodSnapshot controller for member id: it
// binds the member's PodSnapshot to a content with contentConditions, and
// gives the PodSnapshot snapConditions.
func pgReport(t *testing.T, r *PodGroupSnapshotReconciler, id string, contentConditions, snapConditions []metav1.Condition) {
	t.Helper()
	ctx := context.Background()
	snap := &snapshotv1alpha1.PodSnapshot{}
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: pgNamespace, Name: "group-" + id}, snap))

	content := &snapshotv1alpha1.PodSnapshotContent{}
	err := r.Get(ctx, types.NamespacedName{Name: "content-" + id}, content)
	if apierrors.IsNotFound(err) {
		content = &snapshotv1alpha1.PodSnapshotContent{
			ObjectMeta: metav1.ObjectMeta{Name: "content-" + id},
			Spec: snapshotv1alpha1.PodSnapshotContentSpec{
				PodSnapshotRef: snapshotv1alpha1.PodSnapshotReference{Namespace: pgNamespace, Name: snap.Name, UID: snap.UID},
				Source: snapshotv1alpha1.PodSnapshotContentSource{
					PodRef: snap.Spec.Source.PodRef, NodeName: "node-1",
				},
			},
		}
		require.NoError(t, r.Create(ctx, content))
	} else {
		require.NoError(t, err)
	}
	content.Status.Conditions = contentConditions
	require.NoError(t, r.Status().Update(ctx, content))

	snap.Status.BoundPodSnapshotContentName = ptr.To(content.Name)
	snap.Status.Conditions = snapConditions
	require.NoError(t, r.Status().Update(ctx, snap))
}

func TestPodGroupSnapshotReconciler_AddsFinalizerFirst(t *testing.T) {
	group := pgGroup("a")
	group.Finalizers = nil
	r, _ := makePodGroupReconciler(t, append(pgReadyCluster("a"), group)...)

	reconcileGroup(t, r)

	assert.Equal(t, []string{snapshotv1alpha1.PodGroupInUseFinalizer}, getGroup(t, r).Finalizers)
	assert.Empty(t, listMemberPodSnapshots(t, r), "the finalizer pass creates nothing")
}

func TestPodGroupSnapshotReconciler_CreatesMembersWithTheirOwnSettings(t *testing.T) {
	group := pgGroup("head", "worker")
	group.Spec.Members[0].PodSnapshotTemplate = &snapshotv1alpha1.PodGroupMemberTemplate{
		Metadata: &snapshotv1alpha1.PodSnapshotTemplateMetadata{
			Labels:      map[string]string{"role": "head"},
			Annotations: map[string]string{"note": "serves HTTP"},
		},
		Containers: containerSettings("main"),
	}
	group.Spec.Members[1].PodSnapshotTemplate = &snapshotv1alpha1.PodGroupMemberTemplate{
		Metadata: &snapshotv1alpha1.PodSnapshotTemplateMetadata{Labels: map[string]string{"role": "worker"}},
	}
	objs := append(pgReadyCluster("head"), group, pgSourcePod("worker", "node-2"), pgAgent("node-2", true, ptr.To("v1,v2")))
	r, recorder := makePodGroupReconciler(t, objs...)

	res := reconcileGroup(t, r)

	snaps := listMemberPodSnapshots(t, r)
	require.Len(t, snaps, 2)
	byName := map[string]snapshotv1alpha1.PodSnapshot{snaps[0].Name: snaps[0], snaps[1].Name: snaps[1]}
	head, worker := byName["group-head"], byName["group-worker"]
	for _, snap := range []snapshotv1alpha1.PodSnapshot{head, worker} {
		require.NotEmpty(t, snap.Name)
		assert.True(t, metav1.IsControlledBy(&snap, group), snap.Name)
		assert.Equal(t, "group", snap.Labels[snapshotv1alpha1.PodGroupLabel], snap.Name)
	}
	assert.Equal(t, "head", head.Labels[snapshotv1alpha1.PodGroupMemberLabel])
	assert.Equal(t, "head", head.Labels["role"])
	assert.Equal(t, map[string]string{"note": "serves HTTP"}, head.Annotations)
	assert.Equal(t, group.Spec.Members[0].Source, head.Spec.Source)
	assert.Equal(t, containerSettings("main"), head.Spec.Containers)

	assert.Equal(t, "worker", worker.Labels[snapshotv1alpha1.PodGroupMemberLabel])
	assert.Equal(t, "worker", worker.Labels["role"])
	assert.Empty(t, worker.Annotations)
	assert.Equal(t, group.Spec.Members[1].Source, worker.Spec.Source)
	assert.Nil(t, worker.Spec.Containers)

	got := getGroup(t, r)
	admitted := groupCondition(t, got, snapshotv1alpha1.PodGroupConditionAdmitted)
	require.NotNil(t, admitted)
	assert.Equal(t, snapshotv1alpha1.PodGroupReasonAdmitting, admitted.Reason)
	assert.InDelta(t, time.Until(got.CreationTimestamp.Add(600*time.Second)).Seconds(), res.RequeueAfter.Seconds(), 5,
		"requeues at the deadline")

	// The PodSnapshots' events pin them.
	reconcileGroup(t, r)
	got = getGroup(t, r)
	require.Len(t, got.Status.Members, 2)
	for i, m := range got.Status.Members {
		assert.Equal(t, snapshotv1alpha1.CheckpointPhaseAdmitted, m.Phase, m.ID)
		assert.Equal(t, int32(i), m.Rank, m.ID)
		require.NotNil(t, m.PodSnapshot, m.ID)
		assert.Equal(t, "group-"+m.ID, m.PodSnapshot.Name)
		assert.Equal(t, types.UID("group-"+m.ID+"-uid"), m.PodSnapshot.UID)
	}
	assert.Equal(t, "node-1", got.Status.Members[0].NodeName)
	assert.Equal(t, "node-2", got.Status.Members[1].NodeName)
	assert.Empty(t, drainEvents(recorder), "nothing has turned True yet")
}

func TestPodGroupSnapshotReconciler_RefusesBeforeCreatingAnything(t *testing.T) {
	otherSnap := func() *snapshotv1alpha1.PodSnapshot {
		return &snapshotv1alpha1.PodSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "group-b", Namespace: pgNamespace, UID: "other"}}
	}
	for _, tc := range []struct {
		name       string
		mutate     func(group *snapshotv1alpha1.PodGroupSnapshot) []client.Object
		wantReason string
		wantInMsg  string
	}{
		{
			name: "duplicate source Pods",
			mutate: func(group *snapshotv1alpha1.PodGroupSnapshot) []client.Object {
				group.Spec.Members[1].Source = group.Spec.Members[0].Source
				return nil
			},
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantInMsg:  `members "a" and "b" name the same source Pod "pod-a"`,
		},
		{
			name: "a missing source Pod",
			mutate: func(group *snapshotv1alpha1.PodGroupSnapshot) []client.Object {
				group.Spec.Members[1].Source.PodRef.Name = "gone"
				return nil
			},
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantInMsg:  `member "b": source Pod "gone" not found`,
		},
		{
			name: "a UID mismatch",
			mutate: func(group *snapshotv1alpha1.PodGroupSnapshot) []client.Object {
				group.Spec.Members[1].Source.PodRef.UID = "recreated"
				return nil
			},
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantInMsg:  `member "b": ` + errPodSnapshotStalePodRef.Error(),
		},
		{
			name: "a name collision",
			mutate: func(*snapshotv1alpha1.PodGroupSnapshot) []client.Object {
				return []client.Object{otherSnap()}
			},
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantInMsg:  `member "b": PodSnapshot "group-b" already exists and does not belong to this PodGroupSnapshot`,
		},
		{
			name: "a name collision with a PodSnapshot another PodGroupSnapshot controls",
			mutate: func(group *snapshotv1alpha1.PodGroupSnapshot) []client.Object {
				other := group.DeepCopy()
				other.Name, other.UID = "other", "other-uid"
				snap := buildMemberPodSnapshot(other, 1)
				snap.Name = "group-b"
				return []client.Object{snap}
			},
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantInMsg:  `PodSnapshot "group-b" already exists`,
		},
		{
			name: "a template that sets the group label",
			mutate: func(group *snapshotv1alpha1.PodGroupSnapshot) []client.Object {
				group.Spec.Members[1].PodSnapshotTemplate = &snapshotv1alpha1.PodGroupMemberTemplate{
					Metadata: &snapshotv1alpha1.PodSnapshotTemplateMetadata{Labels: map[string]string{snapshotv1alpha1.PodGroupLabel: "x"}},
				}
				return nil
			},
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantInMsg:  "spec.members[1].podSnapshotTemplate.metadata.labels[" + snapshotv1alpha1.PodGroupLabel + "]: Forbidden: label is controller-owned",
		},
		{
			name: "a template that sets the SnapshotJob label",
			mutate: func(group *snapshotv1alpha1.PodGroupSnapshot) []client.Object {
				group.Spec.Members[1].PodSnapshotTemplate = &snapshotv1alpha1.PodGroupMemberTemplate{
					Metadata: &snapshotv1alpha1.PodSnapshotTemplateMetadata{Labels: map[string]string{snapshotv1alpha1.SnapshotJobOwnerLabel: "x"}},
				}
				return nil
			},
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantInMsg:  "spec.members[1].podSnapshotTemplate.metadata.labels[" + snapshotv1alpha1.SnapshotJobOwnerLabel + "]: Forbidden: label is controller-owned",
		},
		{
			name: "a template that sets the SnapshotJob UID label",
			mutate: func(group *snapshotv1alpha1.PodGroupSnapshot) []client.Object {
				group.Spec.Members[1].PodSnapshotTemplate = &snapshotv1alpha1.PodGroupMemberTemplate{
					Metadata: &snapshotv1alpha1.PodSnapshotTemplateMetadata{Labels: map[string]string{snapshotv1alpha1.SnapshotJobOwnerUIDLabel: "x"}},
				}
				return nil
			},
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantInMsg:  "spec.members[1].podSnapshotTemplate.metadata.labels[" + snapshotv1alpha1.SnapshotJobOwnerUIDLabel + "]: Forbidden: label is controller-owned",
		},
		{
			name: "a template label the PodSnapshot cannot carry",
			mutate: func(group *snapshotv1alpha1.PodGroupSnapshot) []client.Object {
				group.Spec.Members[1].PodSnapshotTemplate = &snapshotv1alpha1.PodGroupMemberTemplate{
					Metadata: &snapshotv1alpha1.PodSnapshotTemplateMetadata{Labels: map[string]string{"role": "not a value"}},
				}
				return nil
			},
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantInMsg:  `spec.members[1].podSnapshotTemplate.metadata.labels`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := pgGroup("a", "b")
			extra := tc.mutate(group)
			// No agent runs anywhere: a refusal never waits for one.
			objs := append([]client.Object{group, pgSourcePod("a", "node-1"), pgSourcePod("b", "node-1")}, extra...)
			r, recorder := makePodGroupReconciler(t, objs...)

			res := reconcileGroup(t, r)

			assert.Zero(t, res.RequeueAfter)
			snaps := listMemberPodSnapshots(t, r)
			assert.Len(t, snaps, len(extra), "nothing is created")
			got := getGroup(t, r)
			failed := groupCondition(t, got, snapshotv1alpha1.PodGroupConditionFailed)
			require.NotNil(t, failed)
			assert.Equal(t, tc.wantReason, failed.Reason)
			assert.Contains(t, failed.Message, tc.wantInMsg)
			admitted := groupCondition(t, got, snapshotv1alpha1.PodGroupConditionAdmitted)
			require.NotNil(t, admitted)
			assert.Equal(t, metav1.ConditionFalse, admitted.Status)
			assert.Equal(t, tc.wantReason, admitted.Reason)
			for _, m := range got.Status.Members {
				assert.Equal(t, snapshotv1alpha1.CheckpointPhaseFailed, m.Phase, m.ID)
				assert.Equal(t, tc.wantReason, m.Reason, m.ID)
			}
			events := drainEvents(recorder)
			require.Len(t, events, 1)
			assert.True(t, strings.HasPrefix(events[0], "Warning "+tc.wantReason+" "), events[0])

			// Refused is final: nothing changes on the next pass.
			reconcileGroup(t, r)
			assert.Equal(t, got.ResourceVersion, getGroup(t, r).ResourceVersion)
		})
	}
}

func TestPodGroupSnapshotReconciler_AgentGate(t *testing.T) {
	otherRelease := pgAgent("node-1", true, ptr.To("v1"))
	otherRelease.Name = "agent-of-another-release"
	otherRelease.Labels = labels.Set{"app.kubernetes.io/component": "snapshot-agent", "app.kubernetes.io/instance": "other"}

	for _, tc := range []struct {
		name      string
		agents    []client.Object
		wantInMsg string
	}{
		{
			name:      "no agent on the node",
			agents:    []client.Object{pgAgent("node-2", true, ptr.To("v1")), otherRelease},
			wantInMsg: `node "node-1": no node agent Pod runs on it`,
		},
		{
			name:      "an agent that is not Ready",
			agents:    []client.Object{pgAgent("node-1", false, ptr.To("v1"))},
			wantInMsg: `node "node-1": agent Pod "agent-node-1" is not Ready`,
		},
		{
			name:      "an agent without the annotation",
			agents:    []client.Object{pgAgent("node-1", true, nil)},
			wantInMsg: `node "node-1": agent Pod "agent-node-1" does not support pod groups`,
		},
		{
			name:      "an agent that supports only v2",
			agents:    []client.Object{pgAgent("node-1", true, ptr.To("v2"))},
			wantInMsg: `node "node-1": agent Pod "agent-node-1" supports group protocols "v2", not v1`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := append([]client.Object{pgGroup("a", "b"), pgSourcePod("a", "node-1"), pgSourcePod("b", "node-1")}, tc.agents...)
			r, recorder := makePodGroupReconciler(t, objs...)

			res := reconcileGroup(t, r)

			assert.Equal(t, podGroupAdmissionRequeue, res.RequeueAfter)
			assert.Empty(t, listMemberPodSnapshots(t, r))
			got := getGroup(t, r)
			admitted := groupCondition(t, got, snapshotv1alpha1.PodGroupConditionAdmitted)
			require.NotNil(t, admitted)
			assert.Equal(t, metav1.ConditionFalse, admitted.Status)
			assert.Equal(t, snapshotv1alpha1.PodGroupReasonAgentNotReady, admitted.Reason)
			assert.Equal(t, "waiting for the node agent on 1 node(s): "+tc.wantInMsg, admitted.Message)
			assert.Nil(t, groupCondition(t, got, snapshotv1alpha1.PodGroupConditionFailed))
			for _, m := range got.Status.Members {
				assert.Equal(t, snapshotv1alpha1.CheckpointPhasePending, m.Phase, m.ID)
			}
			assert.Empty(t, drainEvents(recorder))
		})
	}

	t.Run("an agent that supports v1 and v2 passes", func(t *testing.T) {
		r, _ := makePodGroupReconciler(t, pgGroup("a", "b"), pgSourcePod("a", "node-1"), pgSourcePod("b", "node-2"),
			pgAgent("node-1", true, ptr.To("v1,v2")), pgAgent("node-2", true, ptr.To("v2, v1")))
		reconcileGroup(t, r)
		assert.Len(t, listMemberPodSnapshots(t, r), 2)
	})

	t.Run("every blocking node is named", func(t *testing.T) {
		r, _ := makePodGroupReconciler(t, pgGroup("a", "b", "c"),
			pgSourcePod("a", "node-2"), pgSourcePod("b", "node-1"), pgSourcePod("c", "node-3"),
			pgAgent("node-1", false, ptr.To("v1")), pgAgent("node-3", true, ptr.To("v1")))
		reconcileGroup(t, r)
		admitted := groupCondition(t, getGroup(t, r), snapshotv1alpha1.PodGroupConditionAdmitted)
		require.NotNil(t, admitted)
		assert.Equal(t, `waiting for the node agent on 2 node(s): node "node-1": agent Pod "agent-node-1" is not Ready; `+
			`node "node-2": no node agent Pod runs on it`, admitted.Message)
	})
}

func TestPodGroupSnapshotReconciler_WaitsForAPreviousIncarnation(t *testing.T) {
	group := pgGroup("a", "b")
	previous := group.DeepCopy()
	previous.UID = "previous-group-uid"
	leftover := buildMemberPodSnapshot(previous, 1)
	leftover.UID = "leftover-uid"
	r, recorder := makePodGroupReconciler(t, append(pgReadyCluster("a", "b"), group, leftover)...)

	res := reconcileGroup(t, r)

	assert.InDelta(t, time.Until(group.CreationTimestamp.Add(600*time.Second)).Seconds(), res.RequeueAfter.Seconds(), 5,
		"no polling: the leftover's deletion reconciles the group through Owns, which maps by owner name")
	assert.Len(t, listMemberPodSnapshots(t, r), 1, "nothing is created while the earlier group's PodSnapshot remains")
	got := getGroup(t, r)
	admitted := groupCondition(t, got, snapshotv1alpha1.PodGroupConditionAdmitted)
	require.NotNil(t, admitted)
	assert.Equal(t, snapshotv1alpha1.PodGroupReasonPreviousGroupCleanup, admitted.Reason)
	assert.Equal(t, `member "b": PodSnapshot "group-b" of an earlier PodGroupSnapshot "group" (uid "previous-group-uid") is being deleted`,
		admitted.Message)
	assert.Nil(t, groupCondition(t, got, snapshotv1alpha1.PodGroupConditionFailed))
	assert.Empty(t, drainEvents(recorder))

	// Garbage collection removes it; admission then passes.
	require.NoError(t, r.Delete(context.Background(), leftover))
	reconcileGroup(t, r)
	snaps := listMemberPodSnapshots(t, r)
	require.Len(t, snaps, 2)
	for _, snap := range snaps {
		assert.True(t, metav1.IsControlledBy(&snap, group), snap.Name)
	}
}

func TestPodGroupSnapshotReconciler_WaitsForUnscheduledSourcePod(t *testing.T) {
	r, _ := makePodGroupReconciler(t, append(pgReadyCluster("a"), pgGroup("a", "b"), pgSourcePod("b", ""))...)

	res := reconcileGroup(t, r)

	assert.Equal(t, podGroupAdmissionRequeue, res.RequeueAfter)
	assert.Empty(t, listMemberPodSnapshots(t, r))
	admitted := groupCondition(t, getGroup(t, r), snapshotv1alpha1.PodGroupConditionAdmitted)
	require.NotNil(t, admitted)
	assert.Equal(t, snapshotv1alpha1.PodGroupReasonSourcePodUnscheduled, admitted.Reason)
	assert.Equal(t, `member "b": source Pod "pod-b" is not scheduled to a node yet`, admitted.Message)
}

func TestPodGroupSnapshotReconciler_DeadlineWhileWaiting(t *testing.T) {
	group := pgGroup("a")
	group.Spec.DeadlineSeconds = 120
	r, recorder := makePodGroupReconciler(t, group, pgSourcePod("a", "node-1"))
	res := reconcileGroup(t, r)
	require.Equal(t, podGroupAdmissionRequeue, res.RequeueAfter)

	// Move the creation time back past the deadline.
	got := getGroup(t, r)
	got.CreationTimestamp = metav1.NewTime(got.CreationTimestamp.Add(-time.Hour))
	require.NoError(t, r.Update(context.Background(), got))
	res = reconcileGroup(t, r)

	assert.Zero(t, res.RequeueAfter)
	got = getGroup(t, r)
	failed := groupCondition(t, got, snapshotv1alpha1.PodGroupConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, snapshotv1alpha1.PodGroupReasonDeadlineExceeded, failed.Reason)
	assert.Equal(t, `the deadline of 120 seconds passed while admission was waiting: `+
		`waiting for the node agent on 1 node(s): node "node-1": no node agent Pod runs on it`, failed.Message)
	assert.Equal(t, snapshotv1alpha1.CheckpointPhaseFailed, got.Status.Members[0].Phase)
	assert.Empty(t, listMemberPodSnapshots(t, r))
	assert.Equal(t, []string{"Warning DeadlineExceeded " + failed.Message}, drainEvents(recorder))
}

func TestPodGroupSnapshotReconciler_RestartMidwayCreatesTheRest(t *testing.T) {
	group := pgGroup("a", "b", "c")
	existing := buildMemberPodSnapshot(group, 0)
	existing.UID = "group-a-uid"
	// Since member a was created, b's source Pod disappeared and every agent
	// left: admission would refuse or wait now, but it must not run again.
	r, _ := makePodGroupReconciler(t, group, existing, pgSourcePod("a", "node-1"), pgSourcePod("c", "node-1"))

	reconcileGroup(t, r)

	snaps := listMemberPodSnapshots(t, r)
	require.Len(t, snaps, 3)
	got := getGroup(t, r)
	assert.Nil(t, groupCondition(t, got, snapshotv1alpha1.PodGroupConditionFailed))
	admitted := groupCondition(t, got, snapshotv1alpha1.PodGroupConditionAdmitted)
	require.NotNil(t, admitted)
	assert.Equal(t, snapshotv1alpha1.PodGroupReasonAdmitting, admitted.Reason)
	assert.Equal(t, snapshotv1alpha1.CheckpointPhaseAdmitted, got.Status.Members[0].Phase)
}

// TestPodGroupSnapshotReconciler_NeverRefusesAGroupWhoseMemberWorkExists
// confirms a refusal through the API server. The cache has not seen member a's
// PodSnapshot yet, and shows a's source Pod recreated, which alone would refuse
// the group; the API server has the PodSnapshot, and no source Pod. The pass
// through the API server is the one recorded and acted on, so member a is
// pinned and the rest of the member work is created at once.
func TestPodGroupSnapshotReconciler_NeverRefusesAGroupWhoseMemberWorkExists(t *testing.T) {
	group := pgGroup("a", "b")
	memberA := buildMemberPodSnapshot(group, 0)
	memberA.UID = "group-a-uid"
	recreated := pgSourcePod("a", "node-1")
	recreated.UID = "recreated"
	r, recorder := makePodGroupReconciler(t, group.DeepCopy(), recreated, pgSourcePod("b", "node-1"),
		pgAgent("node-1", true, ptr.To("v1")))
	live, _ := makePodGroupReconciler(t, group.DeepCopy(), memberA.DeepCopy(), pgSourcePod("b", "node-1"),
		pgAgent("node-1", true, ptr.To("v1")))
	r.NonCacheReadClient = live.Client

	reconcileGroup(t, r)

	got := getGroup(t, r)
	assert.Nil(t, groupCondition(t, got, snapshotv1alpha1.PodGroupConditionFailed), "member work exists")
	for _, m := range got.Status.Members {
		assert.NotEqual(t, snapshotv1alpha1.CheckpointPhaseFailed, m.Phase, m.ID)
	}
	assert.Equal(t, snapshotv1alpha1.CheckpointPhaseAdmitted, got.Status.Members[0].Phase)
	assert.Equal(t, &snapshotv1alpha1.ObjectIdentity{Name: "group-a", UID: "group-a-uid"}, got.Status.Members[0].PodSnapshot)
	snaps := listMemberPodSnapshots(t, r)
	require.Len(t, snaps, 1, "member a's PodSnapshot is the API server's; only member b's is created")
	assert.Equal(t, "group-b", snaps[0].Name)
	assert.Empty(t, drainEvents(recorder))

	// The cache catches up: nothing changes.
	require.NoError(t, r.Create(context.Background(), memberA.DeepCopy()))
	reconcileGroup(t, r)
	got = getGroup(t, r)
	assert.Nil(t, groupCondition(t, got, snapshotv1alpha1.PodGroupConditionFailed))
	assert.Equal(t, snapshotv1alpha1.CheckpointPhaseAdmitted, got.Status.Members[0].Phase)
	assert.Equal(t, &snapshotv1alpha1.ObjectIdentity{Name: "group-a", UID: "group-a-uid"}, got.Status.Members[0].PodSnapshot)
	assert.Len(t, listMemberPodSnapshots(t, r), 2)
}

func TestPodGroupSnapshotReconciler_KeepsTheReadErrorAfterANameRace(t *testing.T) {
	group := pgGroup("a")
	r, _ := makePodGroupReconciler(t, append(pgReadyCluster("a"), group)...)
	resource := snapshotv1alpha1.GroupVersion.WithResource("podsnapshots").GroupResource()
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
			return apierrors.NewAlreadyExists(resource, obj.GetName())
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*snapshotv1alpha1.PodSnapshot); ok {
				return apierrors.NewForbidden(resource, key.Name, errors.New("denied"))
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	err := r.createMemberPodSnapshot(context.Background(), group, 0)

	require.Error(t, err)
	assert.True(t, apierrors.IsForbidden(err), "the read's error is kept: %v", err)
	assert.False(t, apierrors.IsAlreadyExists(err), "the create's AlreadyExists is not: %v", err)
}

func TestPodGroupSnapshotReconciler_DeletedPodSnapshotFailsItsMember(t *testing.T) {
	r, recorder := makePodGroupReconciler(t, append(pgReadyCluster("a", "b"), pgGroup("a", "b"))...)
	reconcileGroup(t, r)
	reconcileGroup(t, r)
	require.Len(t, listMemberPodSnapshots(t, r), 2)
	ctx := context.Background()
	memberB := &snapshotv1alpha1.PodSnapshot{}
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: pgNamespace, Name: "group-b"}, memberB))

	t.Run("a cache miss alone changes nothing", func(t *testing.T) {
		live := r.Client
		r.Client = interceptor.NewClient(live.(client.WithWatch), interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*snapshotv1alpha1.PodSnapshot); ok && key.Name == "group-b" {
					return apierrors.NewNotFound(snapshotv1alpha1.GroupVersion.WithResource("podsnapshots").GroupResource(), key.Name)
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
		defer func() { r.Client = live }()

		reconcileGroup(t, r)
		assert.Equal(t, snapshotv1alpha1.CheckpointPhaseAdmitted, getGroup(t, r).Status.Members[1].Phase)
	})

	require.NoError(t, r.Delete(ctx, memberB))
	res := reconcileGroup(t, r)

	snaps := listMemberPodSnapshots(t, r)
	require.Len(t, snaps, 1, "a member is never recreated")
	assert.Equal(t, "group-a", snaps[0].Name)
	got := getGroup(t, r)
	assert.Equal(t, snapshotv1alpha1.CheckpointPhaseFailed, got.Status.Members[1].Phase)
	assert.Equal(t, snapshotv1alpha1.ReasonPodSnapshotDeleted, got.Status.Members[1].Reason)
	assert.Equal(t, `PodSnapshot "group-b" was deleted before the member was checkpointed`, got.Status.Members[1].Message)
	assert.Nil(t, groupCondition(t, got, snapshotv1alpha1.PodGroupConditionFailed),
		"the operator does not fail the group: member a's agent reports, or the deadline ends it")
	assert.Positive(t, res.RequeueAfter, "the deadline still applies")
	assert.Empty(t, drainEvents(recorder))
}

// TestPodGroupSnapshotReconciler_Lifecycle drives a group from admission to
// Ready and checks, at every step, that a freshly started reconciler derives
// the same status and emits nothing.
func TestPodGroupSnapshotReconciler_Lifecycle(t *testing.T) {
	r, recorder := makePodGroupReconciler(t, append(pgReadyCluster("a", "b"), pgGroup("a", "b"))...)
	ready := pgCondition(snapshotv1alpha1.PodSnapshotConditionReady, "Ready")

	restartChecks := func(step string) {
		t.Helper()
		before := getGroup(t, r)
		fresh, freshRecorder := makePodGroupReconciler(t)
		fresh.Client, fresh.NonCacheReadClient = r.Client, r.NonCacheReadClient
		reconcileGroup(t, fresh)
		assert.Equal(t, before.ResourceVersion, getGroup(t, r).ResourceVersion, step)
		assert.Empty(t, drainEvents(freshRecorder), step)
	}

	reconcileGroup(t, r)
	reconcileGroup(t, r)
	restartChecks("admitting")

	pgReport(t, r, "a", []metav1.Condition{pgStarted}, nil)
	reconcileGroup(t, r)
	got := getGroup(t, r)
	assert.Equal(t, snapshotv1alpha1.CheckpointPhaseCheckpointing, got.Status.Members[0].Phase)
	assert.Equal(t, pgStartedAt.Unix(), got.Status.Members[0].StartedAt.Unix())
	assert.Equal(t, []string{"Normal Committed every member passed admission, and the checkpoint has started"},
		drainEvents(recorder))
	restartChecks("committed")

	pgReport(t, r, "a", []metav1.Condition{pgStarted, ready}, []metav1.Condition{ready})
	pgReport(t, r, "b", []metav1.Condition{pgStarted, ready}, []metav1.Condition{ready})
	res := reconcileGroup(t, r)
	assert.Zero(t, res.RequeueAfter, "no deadline requeue once every member is final")
	got = getGroup(t, r)
	readyCond := groupCondition(t, got, snapshotv1alpha1.PodGroupConditionReady)
	require.NotNil(t, readyCond)
	assert.Equal(t, metav1.ConditionTrue, readyCond.Status)
	for _, m := range got.Status.Members {
		assert.Equal(t, snapshotv1alpha1.CheckpointPhaseCheckpointed, m.Phase, m.ID)
		assert.Equal(t, &snapshotv1alpha1.ObjectIdentity{Name: "content-" + m.ID, UID: types.UID("content-" + m.ID + "-uid")}, m.Content)
	}
	assert.Equal(t, []string{"Normal Checkpointed all 2 members are checkpointed"}, drainEvents(recorder))
	restartChecks("ready")

	require.NoError(t, r.Delete(context.Background(),
		&snapshotv1alpha1.PodSnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "content-b"}}))
	reconcileGroup(t, r)
	readyCond = groupCondition(t, getGroup(t, r), snapshotv1alpha1.PodGroupConditionReady)
	require.NotNil(t, readyCond)
	assert.Equal(t, metav1.ConditionFalse, readyCond.Status)
	assert.Equal(t, snapshotv1alpha1.PodGroupReasonArtifactUnavailable, readyCond.Reason)
	assert.Equal(t, []string{`Warning ArtifactUnavailable member "b" (rank 1) cannot be restored: its PodSnapshotContent is gone`},
		drainEvents(recorder))
	assert.Len(t, listMemberPodSnapshots(t, r), 2, "never recaptured")
	restartChecks("withdrawn")
}

func TestPodGroupSnapshotReconciler_MemberFailure(t *testing.T) {
	failed := pgCondition(snapshotv1alpha1.PodSnapshotConditionFailed, "SourcePodGone")
	setup := func(t *testing.T) (*PodGroupSnapshotReconciler, *record.FakeRecorder) {
		r, recorder := makePodGroupReconciler(t, append(pgReadyCluster("a", "b"), pgGroup("a", "b"))...)
		reconcileGroup(t, r)
		reconcileGroup(t, r)

		// Member b fails before any member is seen to start. Member a may
		// still start, so the group is not Refused yet.
		pgReport(t, r, "b", []metav1.Condition{failed}, []metav1.Condition{failed})
		reconcileGroup(t, r)
		got := getGroup(t, r)
		require.Equal(t, snapshotv1alpha1.CheckpointPhaseFailed, got.Status.Members[1].Phase)
		require.Nil(t, groupCondition(t, got, snapshotv1alpha1.PodGroupConditionFailed))
		require.Empty(t, drainEvents(recorder))
		return r, recorder
	}

	t.Run("then a's Started arrives: MemberFailed", func(t *testing.T) {
		r, recorder := setup(t)
		pgReport(t, r, "a", []metav1.Condition{pgStarted}, nil)
		reconcileGroup(t, r)

		cond := groupCondition(t, getGroup(t, r), snapshotv1alpha1.PodGroupConditionFailed)
		require.NotNil(t, cond)
		assert.Equal(t, snapshotv1alpha1.PodGroupReasonMemberFailed, cond.Reason)
		assert.Equal(t, `member "b" (rank 1) failed with SourcePodGone: SourcePodGone message`, cond.Message)
		assert.Equal(t, []string{
			"Normal Committed every member passed admission, and the checkpoint has started",
			"Warning MemberFailed " + cond.Message,
		}, drainEvents(recorder))
	})

	t.Run("then a reports the session's Refused: Refused, naming b", func(t *testing.T) {
		r, recorder := setup(t)
		refused := pgCondition(snapshotv1alpha1.PodSnapshotConditionFailed, "Refused")
		pgReport(t, r, "a", []metav1.Condition{refused}, []metav1.Condition{refused})
		reconcileGroup(t, r)

		cond := groupCondition(t, getGroup(t, r), snapshotv1alpha1.PodGroupConditionFailed)
		require.NotNil(t, cond)
		assert.Equal(t, snapshotv1alpha1.PodGroupReasonRefused, cond.Reason)
		assert.Equal(t, `member "b" (rank 1) failed with SourcePodGone: SourcePodGone message`, cond.Message)
		assert.Equal(t, []string{"Warning Refused " + cond.Message}, drainEvents(recorder))
	})
}

func TestPodGroupSnapshotRequeue(t *testing.T) {
	failed := metav1.Condition{Type: snapshotv1alpha1.PodGroupConditionFailed, Status: metav1.ConditionTrue, Reason: "MemberFailed"}
	now := pgNow
	for _, tc := range []struct {
		name       string
		admission  podGroupAdmissionState
		untilEnd   time.Duration
		conditions []metav1.Condition
		phases     []snapshotv1alpha1.CheckpointPhase
		want       time.Duration
	}{
		{"admission waits", podGroupAdmissionWaiting, time.Hour, nil, []snapshotv1alpha1.CheckpointPhase{"Pending"}, podGroupAdmissionRequeue},
		{"admission waits near the deadline", podGroupAdmissionWaiting, 3 * time.Second, nil,
			[]snapshotv1alpha1.CheckpointPhase{"Pending"}, 3 * time.Second},
		{"a member runs", 0, time.Hour, nil, []snapshotv1alpha1.CheckpointPhase{"Checkpointing"}, time.Hour},
		{"failed while a member runs", 0, time.Hour, []metav1.Condition{failed},
			[]snapshotv1alpha1.CheckpointPhase{"Failed", "Checkpointing"}, time.Hour},
		{"ended", 0, time.Hour, []metav1.Condition{failed}, []snapshotv1alpha1.CheckpointPhase{"Failed", "Failed"}, 0},
		{"past the deadline", 0, -time.Second, nil, []snapshotv1alpha1.CheckpointPhase{"Checkpointing"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := &snapshotv1alpha1.PodGroupSnapshotStatus{Conditions: tc.conditions}
			for _, phase := range tc.phases {
				status.Members = append(status.Members, snapshotv1alpha1.PodGroupSnapshotMemberStatus{Phase: phase})
			}
			observed := podGroupSnapshotObservation{admission: podGroupAdmission{state: tc.admission}, deadline: now.Add(tc.untilEnd)}
			assert.Equal(t, tc.want, podGroupSnapshotRequeue(observed, status, now))
		})
	}
}

func TestPodGroupSnapshotReconciler_Deletion(t *testing.T) {
	ready := pgCondition(snapshotv1alpha1.PodSnapshotConditionReady, "Ready")
	setup := func(t *testing.T) *PodGroupSnapshotReconciler {
		r, _ := makePodGroupReconciler(t, append(pgReadyCluster("a", "b"), pgGroup("a", "b"))...)
		reconcileGroup(t, r)
		reconcileGroup(t, r)
		return r
	}
	deleteGroup := func(t *testing.T, r *PodGroupSnapshotReconciler) {
		require.NoError(t, r.Delete(context.Background(), getGroup(t, r)))
	}
	gone := func(t *testing.T, r *PodGroupSnapshotReconciler) bool {
		err := r.Get(context.Background(), types.NamespacedName{Namespace: pgNamespace, Name: "group"}, &snapshotv1alpha1.PodGroupSnapshot{})
		return apierrors.IsNotFound(err)
	}

	t.Run("cancelled before any member starts", func(t *testing.T) {
		r := setup(t)
		var recorded *metav1.Condition
		r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				group := obj.(*snapshotv1alpha1.PodGroupSnapshot)
				recorded = meta.FindStatusCondition(group.Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed)
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		})
		deleteGroup(t, r)
		recorder := r.Recorder.(*record.FakeRecorder)
		drainEvents(recorder)

		reconcileGroup(t, r)

		require.NotNil(t, recorded, "Cancelled is recorded before the finalizer goes")
		assert.Equal(t, snapshotv1alpha1.PodGroupReasonCancelled, recorded.Reason)
		assert.True(t, gone(t, r))
		assert.Equal(t, []string{"Warning Cancelled " + podGroupCancelledMessage}, drainEvents(recorder))
	})

	t.Run("held while a started member runs", func(t *testing.T) {
		r := setup(t)
		pgReport(t, r, "a", []metav1.Condition{pgStarted}, nil)
		deleteGroup(t, r)

		res := reconcileGroup(t, r)

		assert.False(t, gone(t, r))
		got := getGroup(t, r)
		assert.Nil(t, groupCondition(t, got, snapshotv1alpha1.PodGroupConditionFailed), "deletion does not fail a started attempt")
		assert.Positive(t, res.RequeueAfter, "the deadline still applies")
		assert.Len(t, listMemberPodSnapshots(t, r), 2, "nothing is created or removed")

		t.Run("released once Ready", func(t *testing.T) {
			pgReport(t, r, "a", []metav1.Condition{pgStarted, ready}, []metav1.Condition{ready})
			pgReport(t, r, "b", []metav1.Condition{pgStarted, ready}, []metav1.Condition{ready})
			reconcileGroup(t, r)
			assert.True(t, gone(t, r))
		})
	})

	t.Run("foreground deletion of a committed group fails it at once", func(t *testing.T) {
		r := setup(t)
		recorder := r.Recorder.(*record.FakeRecorder)
		pgReport(t, r, "a", []metav1.Condition{pgStarted}, nil)
		pgReport(t, r, "b", []metav1.Condition{pgStarted}, nil)
		reconcileGroup(t, r)
		deleteGroup(t, r)
		drainEvents(recorder)

		// Garbage collection deletes the member PodSnapshots while the
		// finalizer holds the group, a first.
		ctx := context.Background()
		deleteMember := func(id string) {
			require.NoError(t, r.Delete(ctx, &snapshotv1alpha1.PodSnapshot{
				ObjectMeta: metav1.ObjectMeta{Name: "group-" + id, Namespace: pgNamespace},
			}))
		}
		deleteMember("a")
		reconcileGroup(t, r)

		require.False(t, gone(t, r), "member b is still Checkpointing")
		got := getGroup(t, r)
		cond := groupCondition(t, got, snapshotv1alpha1.PodGroupConditionFailed)
		require.NotNil(t, cond)
		assert.Equal(t, snapshotv1alpha1.PodGroupReasonMemberFailed, cond.Reason)
		assert.Equal(t, `member "a" (rank 0) failed with PodSnapshotDeleted: `+
			`PodSnapshot "group-a" was deleted before the member was checkpointed`, cond.Message)
		assert.Equal(t, snapshotv1alpha1.CheckpointPhaseCheckpointing, got.Status.Members[1].Phase)
		assert.Equal(t, []string{"Warning MemberFailed " + cond.Message}, drainEvents(recorder))

		deleteMember("b")
		reconcileGroup(t, r)
		assert.True(t, gone(t, r), "the finalizer goes once no member is Checkpointing")
	})

	t.Run("held while a member dumps after the group failed, released once it ends", func(t *testing.T) {
		r := setup(t)
		failed := pgCondition(snapshotv1alpha1.PodSnapshotConditionFailed, "CriuDumpFailed")
		pgReport(t, r, "a", []metav1.Condition{pgStarted, failed}, []metav1.Condition{failed})
		pgReport(t, r, "b", []metav1.Condition{pgStarted}, nil)
		deleteGroup(t, r)

		reconcileGroup(t, r)
		require.False(t, gone(t, r))
		cond := groupCondition(t, getGroup(t, r), snapshotv1alpha1.PodGroupConditionFailed)
		require.NotNil(t, cond)
		assert.Equal(t, snapshotv1alpha1.PodGroupReasonMemberFailed, cond.Reason)

		lost := pgCondition(snapshotv1alpha1.PodSnapshotConditionFailed, "CoordinatorLost")
		pgReport(t, r, "b", []metav1.Condition{pgStarted, lost}, []metav1.Condition{lost})
		reconcileGroup(t, r)
		assert.True(t, gone(t, r))
	})
}

func TestPodGroupSnapshotReconciler_MapsContentToGroup(t *testing.T) {
	group := pgGroup("a")
	member := buildMemberPodSnapshot(group, 0)
	member.UID = "member-uid"
	standalone := &snapshotv1alpha1.PodSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "standalone", Namespace: pgNamespace}}
	r, _ := makePodGroupReconciler(t, group, member, standalone)
	content := func(name string) *snapshotv1alpha1.PodSnapshotContent {
		return &snapshotv1alpha1.PodSnapshotContent{Spec: snapshotv1alpha1.PodSnapshotContentSpec{
			PodSnapshotRef: snapshotv1alpha1.PodSnapshotReference{Namespace: pgNamespace, Name: name},
		}}
	}
	ctx := context.Background()

	assert.Equal(t, []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: pgNamespace, Name: "group"}}},
		r.podSnapshotContentToPodGroupSnapshot(ctx, content("group-a")))
	assert.Empty(t, r.podSnapshotContentToPodGroupSnapshot(ctx, content("standalone")))
	assert.Empty(t, r.podSnapshotContentToPodGroupSnapshot(ctx, content("missing")))
}

// makeLaggingPodGroupReconciler returns a reconciler whose cache holds cached
// while the API server holds live: the cache lags behind the API server. Status
// is recorded in the cache's client.
func makeLaggingPodGroupReconciler(t *testing.T, cached, live []client.Object) (*PodGroupSnapshotReconciler, *record.FakeRecorder) {
	t.Helper()
	r, recorder := makePodGroupReconciler(t, cached...)
	api, _ := makePodGroupReconciler(t, live...)
	r.NonCacheReadClient = api.Client
	return r, recorder
}

// pgMemberObjects returns the PodSnapshot of the member at rank, as the
// controller builds it and pinned as pgPinned pins it, and the content bound to
// it, with the given conditions. Each call returns new objects.
func pgMemberObjects(group *snapshotv1alpha1.PodGroupSnapshot, rank int, snapConditions, contentConditions []metav1.Condition) []client.Object {
	id := group.Spec.Members[rank].ID
	snap := buildMemberPodSnapshot(group, rank)
	snap.UID = types.UID("snap-uid-" + id)
	snap.Status = snapshotv1alpha1.PodSnapshotStatus{
		BoundPodSnapshotContentName: ptr.To("content-" + id),
		Conditions:                  snapConditions,
	}
	content := &snapshotv1alpha1.PodSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "content-" + id, UID: types.UID("content-uid-" + id)},
		Spec: snapshotv1alpha1.PodSnapshotContentSpec{
			PodSnapshotRef: snapshotv1alpha1.PodSnapshotReference{Namespace: pgNamespace, Name: snap.Name, UID: snap.UID},
			Source:         snapshotv1alpha1.PodSnapshotContentSource{PodRef: snap.Spec.Source.PodRef, NodeName: "node-1"},
		},
		Status: snapshotv1alpha1.PodSnapshotContentStatus{Conditions: contentConditions},
	}
	return []client.Object{snap, content}
}

// pgObjects joins groups of objects, copying each, so that two fake clients
// never share one.
func pgObjects(sets ...[]client.Object) []client.Object {
	var out []client.Object
	for _, set := range sets {
		for _, obj := range set {
			out = append(out, obj.DeepCopyObject().(client.Object))
		}
	}
	return out
}

// pgGroupPastDeadline is a committed group created an hour ago, so past its
// deadline, whose persisted status holds members, as an earlier reconcile
// recorded them.
func pgGroupPastDeadline(members ...snapshotv1alpha1.PodGroupSnapshotMemberStatus) *snapshotv1alpha1.PodGroupSnapshot {
	ids := make([]string, len(members))
	for i, m := range members {
		ids[i] = m.ID
	}
	group := pgGroup(ids...)
	group.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	group.Status.Members = members
	group.Status.Conditions = []metav1.Condition{{
		Type: snapshotv1alpha1.PodGroupConditionAdmitted, Status: metav1.ConditionTrue,
		Reason:             snapshotv1alpha1.PodGroupReasonCommitted,
		Message:            podGroupSnapshotKind.committedMessage,
		LastTransitionTime: metav1.NewTime(group.CreationTimestamp.Add(time.Second)),
	}}
	return group
}

// TestPodGroupSnapshotReconciler_ConfirmsTheDeadline: at the deadline, the
// cache still shows member a checkpointing, while the API server has it
// Checkpointed. The status is derived again from the persisted one, so the
// cached pass's DeadlineExceeded for a does not stick; b, unfinished on both,
// still fails.
func TestPodGroupSnapshotReconciler_ConfirmsTheDeadline(t *testing.T) {
	group := pgGroupPastDeadline(
		pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseCheckpointing),
		pgPinned("b", 1, snapshotv1alpha1.CheckpointPhaseCheckpointing),
	)
	started := []metav1.Condition{pgStarted}
	r, recorder := makeLaggingPodGroupReconciler(t,
		pgObjects([]client.Object{group}, pgMemberObjects(group, 0, nil, started), pgMemberObjects(group, 1, nil, started)),
		pgObjects([]client.Object{group},
			pgMemberObjects(group, 0, []metav1.Condition{pgReady}, []metav1.Condition{pgStarted, pgReady}),
			pgMemberObjects(group, 1, nil, started)),
	)

	reconcileGroup(t, r)

	got := getGroup(t, r)
	assert.Equal(t, snapshotv1alpha1.CheckpointPhaseCheckpointed, got.Status.Members[0].Phase, "not failed from the stale view")
	assert.Equal(t, &snapshotv1alpha1.ObjectIdentity{Name: "content-a", UID: "content-uid-a"}, got.Status.Members[0].Content)
	assert.Equal(t, snapshotv1alpha1.CheckpointPhaseFailed, got.Status.Members[1].Phase)
	assert.Equal(t, snapshotv1alpha1.PodGroupReasonDeadlineExceeded, got.Status.Members[1].Reason)
	failed := groupCondition(t, got, snapshotv1alpha1.PodGroupConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, snapshotv1alpha1.PodGroupReasonDeadlineExceeded, failed.Reason)
	assert.Equal(t, []string{"Warning DeadlineExceeded " + failed.Message}, drainEvents(recorder))
}

// TestPodGroupSnapshotReconciler_ConfirmsAMemberOfAFailedGroup: the group
// already failed on member a. At the deadline, the cache still shows member b
// checkpointing, while the API server has it Checkpointed. The group's Failed
// condition does not change, yet b's final phase is still confirmed.
func TestPodGroupSnapshotReconciler_ConfirmsAMemberOfAFailedGroup(t *testing.T) {
	memberA := pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseFailed)
	memberA.StartedAt = pgStartedAt.DeepCopy()
	memberA.Reason, memberA.Message = "CriuDumpFailed", "CriuDumpFailed message"
	group := pgGroupPastDeadline(memberA, pgPinned("b", 1, snapshotv1alpha1.CheckpointPhaseCheckpointing))
	firstCause := metav1.Condition{
		Type: snapshotv1alpha1.PodGroupConditionFailed, Status: metav1.ConditionTrue,
		Reason: snapshotv1alpha1.PodGroupReasonMemberFailed, Message: "the first cause",
		LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Minute).Truncate(time.Second)),
	}
	group.Status.Conditions = append(group.Status.Conditions, firstCause)
	failedA := pgMemberObjects(group, 0, []metav1.Condition{pgFailed("CriuDumpFailed")},
		[]metav1.Condition{pgStarted, pgFailed("CriuDumpFailed")})
	r, recorder := makeLaggingPodGroupReconciler(t,
		pgObjects([]client.Object{group}, failedA, pgMemberObjects(group, 1, nil, []metav1.Condition{pgStarted})),
		pgObjects([]client.Object{group}, failedA,
			pgMemberObjects(group, 1, []metav1.Condition{pgReady}, []metav1.Condition{pgStarted, pgReady})),
	)

	reconcileGroup(t, r)

	got := getGroup(t, r)
	assert.Equal(t, snapshotv1alpha1.CheckpointPhaseCheckpointed, got.Status.Members[1].Phase, "not failed from the stale view")
	failed := groupCondition(t, got, snapshotv1alpha1.PodGroupConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, firstCause.Reason, failed.Reason)
	assert.Equal(t, firstCause.Message, failed.Message)
	assert.Empty(t, drainEvents(recorder))
}

func TestPodGroupSnapshotReconciler_ConfirmsADeletion(t *testing.T) {
	deleting := func() *snapshotv1alpha1.PodGroupSnapshot {
		group := pgGroup("a", "b")
		group.DeletionTimestamp = ptr.To(metav1.Now())
		group.Status.Members = []snapshotv1alpha1.PodGroupSnapshotMemberStatus{
			pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseAdmitted),
			pgPinned("b", 1, snapshotv1alpha1.CheckpointPhaseAdmitted),
		}
		return group
	}

	t.Run("a member the cache has not seen start keeps the group", func(t *testing.T) {
		group := deleting()
		r, recorder := makeLaggingPodGroupReconciler(t,
			pgObjects([]client.Object{group}, pgMemberObjects(group, 0, nil, nil), pgMemberObjects(group, 1, nil, nil)),
			pgObjects([]client.Object{group},
				pgMemberObjects(group, 0, nil, []metav1.Condition{pgStarted}), pgMemberObjects(group, 1, nil, nil)),
		)

		res := reconcileGroup(t, r)

		got := getGroup(t, r)
		assert.Contains(t, got.Finalizers, snapshotv1alpha1.PodGroupInUseFinalizer, "the finalizer is kept")
		assert.Nil(t, groupCondition(t, got, snapshotv1alpha1.PodGroupConditionFailed), "no Cancelled")
		assert.Equal(t, snapshotv1alpha1.CheckpointPhaseCheckpointing, got.Status.Members[0].Phase)
		assert.NotNil(t, got.Status.Members[0].StartedAt)
		assert.Positive(t, res.RequeueAfter, "the deadline still applies")
		assert.Equal(t, []string{"Normal Committed every member passed admission, and the checkpoint has started"},
			drainEvents(recorder))
	})

	t.Run("a group no member started is cancelled and released", func(t *testing.T) {
		group := deleting()
		members := func() []client.Object {
			return pgObjects([]client.Object{group}, pgMemberObjects(group, 0, nil, nil), pgMemberObjects(group, 1, nil, nil))
		}
		r, recorder := makeLaggingPodGroupReconciler(t, members(), members())

		reconcileGroup(t, r)

		err := r.Get(context.Background(), types.NamespacedName{Namespace: pgNamespace, Name: "group"}, &snapshotv1alpha1.PodGroupSnapshot{})
		assert.True(t, apierrors.IsNotFound(err), "released: %v", err)
		assert.Equal(t, []string{"Warning Cancelled " + podGroupCancelledMessage}, drainEvents(recorder))
	})
}

// TestPodGroupSnapshotReconciler_ActsOnTheConfirmedPass: the cache has not seen
// member b's source Pod, which alone would refuse the group; the API server
// has it. The confirmed pass admits the group, and the act step creates the
// member work from it in the same reconcile.
func TestPodGroupSnapshotReconciler_ActsOnTheConfirmedPass(t *testing.T) {
	group := pgGroup("a", "b")
	agent := pgAgent("node-1", true, ptr.To("v1"))
	r, recorder := makeLaggingPodGroupReconciler(t,
		pgObjects([]client.Object{group, pgSourcePod("a", "node-1"), agent}),
		pgObjects([]client.Object{group, pgSourcePod("a", "node-1"), pgSourcePod("b", "node-1"), agent}),
	)

	reconcileGroup(t, r)

	got := getGroup(t, r)
	assert.Nil(t, groupCondition(t, got, snapshotv1alpha1.PodGroupConditionFailed), "not refused from the stale view")
	admitted := groupCondition(t, got, snapshotv1alpha1.PodGroupConditionAdmitted)
	require.NotNil(t, admitted)
	assert.Equal(t, snapshotv1alpha1.PodGroupReasonAdmitting, admitted.Reason)
	assert.Len(t, listMemberPodSnapshots(t, r), 2, "the member work is created")
	assert.Empty(t, drainEvents(recorder))
}

// TestPodGroupSnapshotReconciler_RecordsNothingWhenTheConfirmationFails: a
// live read fails while confirming the deadline, so nothing is recorded.
func TestPodGroupSnapshotReconciler_RecordsNothingWhenTheConfirmationFails(t *testing.T) {
	group := pgGroupPastDeadline(
		pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseCheckpointing),
		pgPinned("b", 1, snapshotv1alpha1.CheckpointPhaseCheckpointing),
	)
	started := []metav1.Condition{pgStarted}
	objs := func() []client.Object {
		return pgObjects([]client.Object{group}, pgMemberObjects(group, 0, nil, started), pgMemberObjects(group, 1, nil, started))
	}
	r, recorder := makeLaggingPodGroupReconciler(t, objs(), objs())
	r.NonCacheReadClient = interceptor.NewClient(r.NonCacheReadClient.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*snapshotv1alpha1.PodSnapshot); ok {
				return apierrors.NewInternalError(errors.New("unavailable"))
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	before := getGroup(t, r)

	_, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Namespace: pgNamespace, Name: "group"}})

	require.Error(t, err)
	assert.True(t, apierrors.IsInternalError(err), "the live read's error is returned: %v", err)
	after := getGroup(t, r)
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "nothing is recorded")
	assert.Equal(t, before.Status, after.Status)
	assert.Empty(t, drainEvents(recorder))
}

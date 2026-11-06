// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// makePodGroupRestoreReconciler builds a reconciler whose cache and API server
// are one fake client.
func makePodGroupRestoreReconciler(t *testing.T, objs ...client.Object) (*PodGroupRestoreReconciler, *record.FakeRecorder) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(snapshotReconcilerScheme()).WithObjects(objs...).
		WithStatusSubresource(&snapshotv1alpha1.PodGroupRestore{}, &snapshotv1alpha1.PodGroupSnapshot{}, &corev1.Pod{}).
		Build()
	recorder := record.NewFakeRecorder(100)
	return &PodGroupRestoreReconciler{
		Client:             c,
		NonCacheReadClient: c,
		Recorder:           recorder,
		AgentNamespace:     pgAgentNamespace,
		AgentSelector:      labels.SelectorFromSet(pgAgentLabels),
	}, recorder
}

// rgSnapshot is a Ready PodGroupSnapshot named "group" whose members a, b, ...
// were checkpointed from container "main".
func rgSnapshot(ids ...string) *snapshotv1alpha1.PodGroupSnapshot {
	group := pgGroup(ids...)
	group.Status.Conditions = []metav1.Condition{{
		Type: snapshotv1alpha1.PodGroupConditionReady, Status: metav1.ConditionTrue, Reason: "Checkpointed",
		LastTransitionTime: metav1.Now(),
	}}
	return group
}

// rgRestore is a PodGroupRestore named "restore" of rgSnapshot whose targets
// a, b, ... name the restore Pods restore-a, restore-b, ...; it already holds
// its finalizer.
func rgRestore(ids ...string) *snapshotv1alpha1.PodGroupRestore {
	restore := &snapshotv1alpha1.PodGroupRestore{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "restore",
			Namespace:         pgNamespace,
			UID:               "restore-uid",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Minute).Truncate(time.Second)),
			Finalizers:        []string{snapshotv1alpha1.PodGroupInUseFinalizer},
		},
		Spec: snapshotv1alpha1.PodGroupRestoreSpec{
			SnapshotRef:     snapshotv1alpha1.ObjectIdentity{Name: "group", UID: "group-uid"},
			DeadlineSeconds: 600,
		},
	}
	for _, id := range ids {
		restore.Spec.Targets = append(restore.Spec.Targets, snapshotv1alpha1.PodGroupRestoreTarget{
			ID: id, PodRef: snapshotv1alpha1.TargetPodReference{Name: "restore-" + id},
		})
	}
	return restore
}

// rgTargetPod is target id's restore Pod on node, bound to "restore", running
// with container "main".
func rgTargetPod(id, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "restore-" + id,
			Namespace: pgNamespace,
			UID:       types.UID("restore-pod-uid-" + id),
			Annotations: map[string]string{
				podcontract.RestoreGroupAnnotation:    "restore",
				podcontract.RestoreGroupUIDAnnotation: "restore-uid",
			},
		},
		Spec: corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			PodIP:             "10.1.0." + strconv.Itoa(int(id[0])),
			ContainerStatuses: []corev1.ContainerStatus{{Name: "main", ContainerID: "containerd://" + id}},
		},
	}
}

// rgReadyCluster returns the snapshot of ids, their target Pods on node-1, and
// a group-capable agent there.
func rgReadyCluster(ids ...string) []client.Object {
	objs := []client.Object{rgSnapshot(ids...), pgAgent("node-1", true, ptr.To("v1"))}
	for _, id := range ids {
		objs = append(objs, rgTargetPod(id, "node-1"))
	}
	return objs
}

// bind sets a Pod's binding to the PodGroupRestore name and uid, leaving out
// the UID annotation when uid is empty.
func bind(pod *corev1.Pod, name, uid string) {
	pod.Annotations = map[string]string{podcontract.RestoreGroupAnnotation: name}
	if uid != "" {
		pod.Annotations[podcontract.RestoreGroupUIDAnnotation] = uid
	}
}

// rgOtherRestore is another PodGroupRestore of the same snapshot, named
// "other" with uid, which pinned target b's Pod, restore-b; conditions are its
// own.
func rgOtherRestore(uid types.UID, conditions ...metav1.Condition) *snapshotv1alpha1.PodGroupRestore {
	other := rgRestore("b")
	other.Name, other.UID = "other", uid
	other.Status.Conditions = conditions
	other.Status.Members = []snapshotv1alpha1.PodGroupRestoreMemberStatus{{
		ID: "b", Phase: snapshotv1alpha1.RestorePhaseAdmitted,
		Pod: &snapshotv1alpha1.ObjectIdentity{Name: "restore-b", UID: "restore-pod-uid-b"},
	}}
	return other
}

func reconcileRestore(t *testing.T, r *PodGroupRestoreReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Namespace: pgNamespace, Name: "restore"}})
	require.NoError(t, err)
	return res
}

func getRestore(t *testing.T, r *PodGroupRestoreReconciler) *snapshotv1alpha1.PodGroupRestore {
	t.Helper()
	restore := &snapshotv1alpha1.PodGroupRestore{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: pgNamespace, Name: "restore"}, restore))
	return restore
}

func restoreGone(t *testing.T, r *PodGroupRestoreReconciler) bool {
	t.Helper()
	err := r.Get(context.Background(), types.NamespacedName{Namespace: pgNamespace, Name: "restore"}, &snapshotv1alpha1.PodGroupRestore{})
	return apierrors.IsNotFound(err)
}

// rgReport plays target id's agent: it reports conditions on the restore Pod
// under this restore's UID.
func rgReport(t *testing.T, r *PodGroupRestoreReconciler, id string, conditions ...corev1.PodCondition) {
	t.Helper()
	rgReportAs(t, r, id, "restore-uid", conditions...)
}

// rgReportAs reports conditions on target id's restore Pod under reportUID,
// or with no report UID when it is empty. The agent writes both in one update
// of the Pod's status, which the fake client cannot: it keeps a status update's
// metadata out, so this writes them in turn.
func rgReportAs(t *testing.T, r *PodGroupRestoreReconciler, id, reportUID string, conditions ...corev1.PodCondition) {
	t.Helper()
	ctx := context.Background()
	pod := &corev1.Pod{}
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: pgNamespace, Name: "restore-" + id}, pod))
	if reportUID == "" {
		delete(pod.Annotations, podcontract.RestoreGroupReportUIDAnnotation)
	} else {
		pod.Annotations[podcontract.RestoreGroupReportUIDAnnotation] = reportUID
	}
	require.NoError(t, r.Update(ctx, pod))
	pod.Status.Conditions = conditions
	require.NoError(t, r.Status().Update(ctx, pod))
}

// countStatusPatches makes r count its status patches.
func countStatusPatches(r *PodGroupRestoreReconciler) *int {
	patches := new(int)
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			*patches++
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
	return patches
}

func TestPodGroupRestoreReconciler_AddsFinalizerFirst(t *testing.T) {
	restore := rgRestore("a")
	restore.Finalizers = nil
	r, _ := makePodGroupRestoreReconciler(t, append(rgReadyCluster("a"), restore)...)

	reconcileRestore(t, r)

	got := getRestore(t, r)
	assert.Equal(t, []string{snapshotv1alpha1.PodGroupInUseFinalizer}, got.Finalizers)
	assert.Empty(t, got.Status.Members, "the finalizer pass pins nothing")
}

// TestPodGroupRestoreReconciler_Lifecycle pins two members on different nodes,
// targets listed out of rank order, in one patch, then follows their agents' reports to Ready, checking at every
// step that a freshly started reconciler derives the same status and emits
// nothing.
func TestPodGroupRestoreReconciler_Lifecycle(t *testing.T) {
	restore := rgRestore("b", "a") // targets in another order than the snapshot's ranks
	objs := []client.Object{rgSnapshot("a", "b"), restore, rgTargetPod("a", "node-1"), rgTargetPod("b", "node-2"),
		pgAgent("node-1", true, ptr.To("v1")), pgAgent("node-2", true, ptr.To("v1,v2"))}
	r, recorder := makePodGroupRestoreReconciler(t, objs...)
	patches := countStatusPatches(r)

	restartChecks := func(step string) {
		t.Helper()
		before := getRestore(t, r)
		fresh, freshRecorder := makePodGroupRestoreReconciler(t)
		fresh.Client, fresh.NonCacheReadClient = r.Client, r.NonCacheReadClient
		reconcileRestore(t, fresh)
		assert.Equal(t, before.ResourceVersion, getRestore(t, r).ResourceVersion, step)
		assert.Empty(t, drainEvents(freshRecorder), step)
	}

	res := reconcileRestore(t, r)

	assert.Equal(t, 1, *patches, "every member is pinned in one status patch")
	got := getRestore(t, r)
	require.Len(t, got.Status.Members, 2)
	for i, id := range []string{"b", "a"} {
		m := got.Status.Members[i]
		assert.Equal(t, id, m.ID, "members are listed in spec order")
		assert.Equal(t, snapshotv1alpha1.RestorePhaseAdmitted, m.Phase, id)
		assert.Equal(t, &snapshotv1alpha1.ObjectIdentity{Name: "restore-" + id, UID: types.UID("restore-pod-uid-" + id)}, m.Pod, id)
		assert.Equal(t, []snapshotv1alpha1.PodGroupRestoreContainerStatus{{Name: "main", ContainerID: "containerd://" + id}},
			m.Containers, id)
		assert.Equal(t, rgTargetPod(id, "").Status.PodIP, m.TargetPodIP, id)
	}
	assert.Equal(t, "node-2", got.Status.Members[0].NodeName)
	assert.Equal(t, "node-1", got.Status.Members[1].NodeName)
	admitted := rgCondition(got.Status.Conditions, snapshotv1alpha1.PodGroupConditionAdmitted)
	require.NotNil(t, admitted)
	assert.Equal(t, snapshotv1alpha1.PodGroupReasonAdmitting, admitted.Reason)
	assert.InDelta(t, time.Until(got.CreationTimestamp.Add(600*time.Second)).Seconds(), res.RequeueAfter.Seconds(), 5,
		"requeues at the deadline")
	assert.Empty(t, drainEvents(recorder))
	restartChecks("pinned")

	t.Run("admission never runs again once pinned", func(t *testing.T) {
		require.NoError(t, r.Delete(context.Background(), pgAgent("node-2", true, nil)))
		reconcileRestore(t, r)
		assert.Equal(t, snapshotv1alpha1.PodGroupReasonAdmitting,
			rgCondition(getRestore(t, r).Status.Conditions, snapshotv1alpha1.PodGroupConditionAdmitted).Reason)
	})

	rgReport(t, r, "a", prStarted)
	reconcileRestore(t, r)
	got = getRestore(t, r)
	assert.Equal(t, snapshotv1alpha1.RestorePhaseRestoring, got.Status.Members[1].Phase)
	assert.Equal(t, pgStartedAt.Unix(), got.Status.Members[1].StartedAt.Unix())
	assert.Equal(t, []string{"Normal Committed every member passed admission, and the restore has started"}, drainEvents(recorder))
	restartChecks("committed")

	rgReport(t, r, "a", prStarted, prReleased)
	rgReport(t, r, "b", prStarted)
	reconcileRestore(t, r)
	assert.Equal(t, []string{"Normal Released the release barrier completed; released members keep running whatever happens to the others"},
		drainEvents(recorder))
	restartChecks("released")

	rgReport(t, r, "b", prStarted, prReleased)
	res = reconcileRestore(t, r)
	assert.Zero(t, res.RequeueAfter, "no deadline requeue once every member is final")
	got = getRestore(t, r)
	ready := rgCondition(got.Status.Conditions, snapshotv1alpha1.PodGroupConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Equal(t, []string{"Normal Released all 2 members are released"}, drainEvents(recorder))
	restartChecks("ready")
}

// rgCondition finds a condition.
func rgCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}

func TestPodGroupRestoreReconciler_RefusesBeforePinningAnything(t *testing.T) {
	// otherRestore is an unfinished restore that pinned member b's target Pod.
	otherRestore := func() *snapshotv1alpha1.PodGroupRestore { return rgOtherRestore("other-uid") }
	onB := func(change func(pod *corev1.Pod)) func(objs []client.Object) {
		return func(objs []client.Object) {
			for _, obj := range objs {
				if pod, ok := obj.(*corev1.Pod); ok && pod.Name == "restore-b" {
					change(pod)
				}
			}
		}
	}
	started := func(reportUID string) func(objs []client.Object) {
		return onB(func(pod *corev1.Pod) {
			pod.Status.Conditions = []corev1.PodCondition{{Type: podcontract.RestoreStartedCondition, Status: corev1.ConditionFalse}}
			if reportUID != "" {
				pod.Annotations[podcontract.RestoreGroupReportUIDAnnotation] = reportUID
			}
		})
	}

	for _, tc := range []struct {
		name       string
		restore    func(restore *snapshotv1alpha1.PodGroupRestore)
		cluster    func(objs []client.Object)
		extra      []client.Object
		wantReason string
		wantMsg    string
	}{
		{
			name:       "the snapshot is missing",
			restore:    func(r *snapshotv1alpha1.PodGroupRestore) { r.Spec.SnapshotRef.Name = "missing" },
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantMsg:    `PodGroupSnapshot "missing" not found`,
		},
		{
			name:       "the snapshot is another object",
			restore:    func(r *snapshotv1alpha1.PodGroupRestore) { r.Spec.SnapshotRef.UID = "old-group-uid" },
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantMsg:    `PodGroupSnapshot "group" has UID "group-uid", not "old-group-uid": it was recreated`,
		},
		{
			name: "the snapshot is being deleted",
			cluster: func(objs []client.Object) {
				objs[0].SetDeletionTimestamp(ptr.To(metav1.Now()))
			},
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantMsg:    `PodGroupSnapshot "group" is being deleted`,
		},
		{
			name: "the snapshot is not Ready",
			cluster: func(objs []client.Object) {
				objs[0].(*snapshotv1alpha1.PodGroupSnapshot).Status.Conditions = []metav1.Condition{{
					Type: snapshotv1alpha1.PodGroupConditionReady, Status: metav1.ConditionFalse, Reason: "ArtifactUnavailable",
					Message: "its PodSnapshotContent is gone", LastTransitionTime: metav1.Now(),
				}}
			},
			wantReason: snapshotv1alpha1.PodGroupReasonArtifactUnavailable,
			wantMsg:    `PodGroupSnapshot "group" is not Ready: its PodSnapshotContent is gone`,
		},
		{
			name:       "a target with no member",
			restore:    func(r *snapshotv1alpha1.PodGroupRestore) { r.Spec.Targets[1].ID = "c" },
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantMsg:    `target "c" matches no member of PodGroupSnapshot "group"`,
		},
		{
			name:       "a member with no target",
			restore:    func(r *snapshotv1alpha1.PodGroupRestore) { r.Spec.Targets = r.Spec.Targets[:1] },
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantMsg:    `member "b" of PodGroupSnapshot "group" has no target`,
		},
		{
			name:       "duplicate target Pods",
			restore:    func(r *snapshotv1alpha1.PodGroupRestore) { r.Spec.Targets[1].PodRef.Name = "restore-a" },
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantMsg:    `targets "a" and "b" name the same Pod "restore-a"`,
		},
		{
			name: "a mapping from another container",
			restore: func(r *snapshotv1alpha1.PodGroupRestore) {
				r.Spec.Targets[1].ContainerMappings = []snapshotv1alpha1.ContainerMapping{{Source: "sidecar", Destination: "main"}}
			},
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantMsg:    `target "b" maps container "sidecar", but member "b" was checkpointed from container "main"`,
		},
		{
			name:       "a target Pod without the annotation",
			cluster:    onB(func(pod *corev1.Pod) { pod.Annotations = nil }),
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantMsg:    `target "b": Pod "restore-b" is not a group target: it has no nvidia.com/restore-group annotation`,
		},
		{
			name:       "a target Pod bound to another unfinished restore",
			cluster:    onB(func(pod *corev1.Pod) { bind(pod, "other", "other-uid") }),
			extra:      []client.Object{otherRestore()},
			wantReason: snapshotv1alpha1.PodGroupReasonTargetInUse,
			wantMsg:    `target "b": Pod "restore-b" is bound to PodGroupRestore "other", which has not finished`,
		},
		{
			name:       "a target Pod an earlier restore started on, with no report UID",
			cluster:    started(""),
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantMsg: `target "b": Pod "restore-b" carries nvidia.com/RestoreStarted: an earlier restore changed it, ` +
				`so restoring needs a new Pod`,
		},
		{
			name:       "a target Pod an earlier restore started on, under its own report UID",
			cluster:    started("earlier-uid"),
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantMsg: `target "b": Pod "restore-b" carries nvidia.com/RestoreStarted: an earlier restore changed it, ` +
				`so restoring needs a new Pod`,
		},
		{
			name: "a target Pod an earlier restore started on, bound to another unfinished restore",
			cluster: func(objs []client.Object) {
				onB(func(pod *corev1.Pod) { bind(pod, "other", "other-uid") })(objs)
				started("other-uid")(objs)
			},
			extra:      []client.Object{otherRestore()},
			wantReason: snapshotv1alpha1.PodGroupReasonTargetInUse,
			wantMsg:    `target "b": Pod "restore-b" is bound to PodGroupRestore "other", which has not finished`,
		},
		{
			name:       "a stale UID",
			restore:    func(r *snapshotv1alpha1.PodGroupRestore) { r.Spec.Targets[1].PodRef.UID = "old-pod-uid" },
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantMsg:    `target "b": Pod "restore-b" has UID "restore-pod-uid-b", not "old-pod-uid": it was recreated`,
		},
		{
			name:       "a UID another unfinished restore pinned",
			extra:      []client.Object{otherRestore()},
			wantReason: snapshotv1alpha1.PodGroupReasonTargetInUse,
			wantMsg:    `target "b": Pod "restore-b" is pinned by PodGroupRestore "other", which has not finished`,
		},
		{
			name:       "an IPv6 address",
			cluster:    onB(func(pod *corev1.Pod) { pod.Status.PodIP = "fd00::2" }),
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantMsg:    `target "b": Pod "restore-b" has the address "fd00::2", but a group restore supports only IPv4`,
		},
		{
			name: "an IPv6 address on a Pod not bound to this restore yet",
			cluster: onB(func(pod *corev1.Pod) {
				bind(pod, "restore", "")
				pod.Status.PodIP = "fd00::2"
			}),
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantMsg:    `target "b": Pod "restore-b" has the address "fd00::2", but a group restore supports only IPv4`,
		},
		{
			name: "a running Pod without the destination container",
			restore: func(r *snapshotv1alpha1.PodGroupRestore) {
				r.Spec.Targets[1].ContainerMappings = []snapshotv1alpha1.ContainerMapping{{Source: "main", Destination: "server"}}
			},
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantMsg:    `target "b": Pod "restore-b" runs but has no container "server" to restore into`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restore := rgRestore("a", "b")
			if tc.restore != nil {
				tc.restore(restore)
			}
			// No agent runs anywhere: a refusal never waits for one.
			objs := []client.Object{rgSnapshot("a", "b"), rgTargetPod("a", "node-1"), rgTargetPod("b", "node-1")}
			if tc.cluster != nil {
				tc.cluster(objs)
			}
			r, recorder := makePodGroupRestoreReconciler(t, append(append(objs, restore), tc.extra...)...)

			res := reconcileRestore(t, r)

			assert.Zero(t, res.RequeueAfter)
			got := getRestore(t, r)
			failed := rgCondition(got.Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed)
			require.NotNil(t, failed)
			assert.Equal(t, tc.wantReason, failed.Reason)
			assert.Equal(t, tc.wantMsg, failed.Message)
			admitted := rgCondition(got.Status.Conditions, snapshotv1alpha1.PodGroupConditionAdmitted)
			require.NotNil(t, admitted)
			assert.Equal(t, metav1.ConditionFalse, admitted.Status)
			assert.Equal(t, tc.wantReason, admitted.Reason)
			require.Len(t, got.Status.Members, len(restore.Spec.Targets))
			for _, m := range got.Status.Members {
				assert.Equal(t, snapshotv1alpha1.RestorePhaseFailed, m.Phase, m.ID)
				assert.Equal(t, tc.wantReason, m.Reason, m.ID)
				assert.Equal(t, podGroupRestoreRefusedMemberMessage, m.Message, m.ID)
				assert.Nil(t, m.Pod, "nothing is pinned")
				assert.Empty(t, m.TargetPodIP, "nothing is pinned")
			}
			assert.True(t, podGroupRestoreFinished(&got.Status))
			events := drainEvents(recorder)
			require.Len(t, events, 1)
			assert.True(t, strings.HasPrefix(events[0], "Warning "+tc.wantReason+" "), events[0])

			// A refusal is final: nothing changes on the next pass.
			reconcileRestore(t, r)
			assert.Equal(t, got.ResourceVersion, getRestore(t, r).ResourceVersion)
		})
	}
}

func TestPodGroupRestoreReconciler_ConfirmsARefusal(t *testing.T) {
	objs := rgReadyCluster("a")
	r, _ := makePodGroupRestoreReconciler(t, append(objs, rgRestore("a"))...)
	// The cache has not seen the target Pod's annotation yet.
	live := r.Client
	r.Client = interceptor.NewClient(live.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if pod, ok := obj.(*corev1.Pod); ok && pod.Name == "restore-a" {
				pod.Annotations = nil
			}
			return nil
		},
	})

	reconcileRestore(t, r)

	got := getRestore(t, r)
	assert.Nil(t, rgCondition(got.Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed), "the API server has the annotation")
	assert.Equal(t, snapshotv1alpha1.RestorePhaseAdmitted, got.Status.Members[0].Phase)
}

// TestPodGroupRestoreReconciler_ConfirmsAPass confirms a pass against the API
// server before it pins anything: the cache has not seen restore "other" pin
// target a's Pod, which the API server has, so the restore is refused with
// TargetInUse although its target is bound to it.
func TestPodGroupRestoreReconciler_ConfirmsAPass(t *testing.T) {
	other := rgOtherRestore("other-uid")
	other.Status.Members[0].ID = "a"
	other.Status.Members[0].Pod = &snapshotv1alpha1.ObjectIdentity{Name: "restore-a", UID: "restore-pod-uid-a"}
	unpinned := other.DeepCopy()
	unpinned.Status.Members = nil
	r, recorder := makePodGroupRestoreReconciler(t, append(rgReadyCluster("a"), rgRestore("a"), unpinned)...)
	live, _ := makePodGroupRestoreReconciler(t, append(rgReadyCluster("a"), rgRestore("a"), other)...)
	r.NonCacheReadClient = live.Client

	reconcileRestore(t, r)

	got := getRestore(t, r)
	failed := rgCondition(got.Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, snapshotv1alpha1.PodGroupReasonTargetInUse, failed.Reason)
	assert.Equal(t, `target "a": Pod "restore-a" is pinned by PodGroupRestore "other", which has not finished`, failed.Message)
	assert.Nil(t, got.Status.Members[0].Pod, "nothing is pinned")
	assert.Equal(t, []string{"Warning TargetInUse " + failed.Message}, drainEvents(recorder))
}

// TestPodGroupRestoreReconciler_ReadsOnlyThisAttemptsReport reuses a Pod that
// still carries an earlier attempt's report: it is pinned, and its member
// reads nothing until this restore's agent reports under this restore's UID.
func TestPodGroupRestoreReconciler_ReadsOnlyThisAttemptsReport(t *testing.T) {
	reused := rgTargetPod("a", "node-1")
	reused.Annotations[podcontract.RestoreGroupReportUIDAnnotation] = "earlier-uid"
	reused.Status.Conditions = []corev1.PodCondition{
		prRestoreError, prGroupFailed(snapshotv1alpha1.PodGroupReasonRefused),
	}
	r, _ := makePodGroupRestoreReconciler(t, rgSnapshot("a"), pgAgent("node-1", true, ptr.To("v1")), reused, rgRestore("a"))

	reconcileRestore(t, r)
	got := getRestore(t, r)
	require.NotNil(t, got.Status.Members[0].Pod, "the reused Pod is pinned")
	assert.Equal(t, snapshotv1alpha1.RestorePhaseAdmitted, got.Status.Members[0].Phase, "the earlier report is not read")
	assert.Nil(t, rgCondition(got.Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed))

	t.Run("a report with no report UID is ignored", func(t *testing.T) {
		rgReportAs(t, r, "a", "", prStarted, prRestoreError)
		reconcileRestore(t, r)
		m := getRestore(t, r).Status.Members[0]
		assert.Equal(t, snapshotv1alpha1.RestorePhaseAdmitted, m.Phase)
		assert.Nil(t, m.StartedAt)
	})

	t.Run("a report under this restore's UID is read", func(t *testing.T) {
		rgReport(t, r, "a", prStarted, prRestoreError, prGroupFailed(snapshotv1alpha1.PodGroupReasonPodIPChanged))
		reconcileRestore(t, r)
		m := getRestore(t, r).Status.Members[0]
		assert.Equal(t, snapshotv1alpha1.RestorePhaseFailed, m.Phase)
		assert.Equal(t, snapshotv1alpha1.PodGroupReasonPodIPChanged, m.Reason)
		assert.NotNil(t, m.StartedAt)
	})
}

func TestPodGroupRestoreReconciler_Waits(t *testing.T) {
	pod := func(mutate func(*corev1.Pod)) []client.Object {
		p := rgTargetPod("b", "node-1")
		mutate(p)
		return []client.Object{p}
	}
	bound := func(name, uid string) []client.Object {
		return pod(func(p *corev1.Pod) { bind(p, name, uid) })
	}
	finished := metav1.Condition{Type: snapshotv1alpha1.PodGroupConditionFailed, Status: metav1.ConditionTrue,
		Reason: "Refused", LastTransitionTime: metav1.Now()}
	const notBoundYet = `target "b": Pod "restore-b" is not bound to this PodGroupRestore yet: `
	for _, tc := range []struct {
		name        string
		targetB     []client.Object
		agents      []client.Object
		extra       []client.Object
		wantReason  string
		wantMsg     string
		wantRequeue func(deadline time.Duration) time.Duration
	}{
		{
			name:       "a target Pod bound to this restore's name without a UID",
			targetB:    bound("restore", ""),
			agents:     []client.Object{pgAgent("node-1", true, ptr.To("v1"))},
			wantReason: snapshotv1alpha1.PodGroupReasonTargetPodPending,
			wantMsg:    notBoundYet + `it names PodGroupRestore "restore" without a UID`,
		},
		{
			name:       "a target Pod bound to an earlier PodGroupRestore that this one recreated under its name",
			targetB:    bound("restore", "earlier-restore-uid"),
			agents:     []client.Object{pgAgent("node-1", true, ptr.To("v1"))},
			wantReason: snapshotv1alpha1.PodGroupReasonTargetPodPending,
			wantMsg: notBoundYet + `it is bound to PodGroupRestore "restore" with UID "earlier-restore-uid", ` +
				`an attempt that has finished or no longer exists`,
		},
		{
			name:       "a target Pod bound to a finished restore",
			targetB:    bound("other", "other-uid"),
			extra:      []client.Object{rgOtherRestore("other-uid", finished)},
			agents:     []client.Object{pgAgent("node-1", true, ptr.To("v1"))},
			wantReason: snapshotv1alpha1.PodGroupReasonTargetPodPending,
			wantMsg: notBoundYet + `it is bound to PodGroupRestore "other" with UID "other-uid", ` +
				`an attempt that has finished or no longer exists`,
		},
		{
			name:       "a target Pod bound to a missing restore",
			targetB:    bound("other", "other-uid"),
			agents:     []client.Object{pgAgent("node-1", true, ptr.To("v1"))},
			wantReason: snapshotv1alpha1.PodGroupReasonTargetPodPending,
			wantMsg: notBoundYet + `it is bound to PodGroupRestore "other" with UID "other-uid", ` +
				`an attempt that has finished or no longer exists`,
		},
		{
			name:    "a target Pod bound to a restore since recreated under the same name",
			targetB: bound("other", "other-uid"),
			extra: []client.Object{func() client.Object {
				recreated := rgOtherRestore("recreated-other-uid")
				recreated.Status.Members = nil // it has pinned nothing yet
				return recreated
			}()},
			agents:     []client.Object{pgAgent("node-1", true, ptr.To("v1"))},
			wantReason: snapshotv1alpha1.PodGroupReasonTargetPodPending,
			wantMsg: notBoundYet + `it is bound to PodGroupRestore "other" with UID "other-uid", ` +
				`an attempt that has finished or no longer exists`,
		},
		{
			name:       "a target Pod that does not exist yet",
			agents:     []client.Object{pgAgent("node-1", true, ptr.To("v1"))},
			wantReason: snapshotv1alpha1.PodGroupReasonTargetPodPending,
			wantMsg:    `target "b": Pod "restore-b" does not exist yet`,
		},
		{
			name:       "an unscheduled target Pod",
			targetB:    pod(func(p *corev1.Pod) { p.Spec.NodeName, p.Status = "", corev1.PodStatus{Phase: corev1.PodPending} }),
			agents:     []client.Object{pgAgent("node-1", true, ptr.To("v1"))},
			wantReason: snapshotv1alpha1.PodGroupReasonTargetPodPending,
			wantMsg:    `target "b": Pod "restore-b" is not scheduled to a node yet`,
		},
		{
			name:       "a target Pod with no address yet",
			targetB:    pod(func(p *corev1.Pod) { p.Status = corev1.PodStatus{Phase: corev1.PodPending} }),
			agents:     []client.Object{pgAgent("node-1", true, ptr.To("v1"))},
			wantReason: snapshotv1alpha1.PodGroupReasonTargetPodPending,
			wantMsg:    `target "b": Pod "restore-b" has no IP address yet`,
		},
		{
			name: "a pending target Pod with no container status yet",
			targetB: pod(func(p *corev1.Pod) {
				p.Status.Phase, p.Status.ContainerStatuses = corev1.PodPending, nil
			}),
			agents:     []client.Object{pgAgent("node-1", true, ptr.To("v1"))},
			wantReason: snapshotv1alpha1.PodGroupReasonTargetPodPending,
			wantMsg:    `target "b": container "main" of Pod "restore-b" has no ID yet`,
		},
		{
			name:       "a destination container with no ID yet",
			targetB:    pod(func(p *corev1.Pod) { p.Status.ContainerStatuses[0].ContainerID = "" }),
			agents:     []client.Object{pgAgent("node-1", true, ptr.To("v1"))},
			wantReason: snapshotv1alpha1.PodGroupReasonTargetPodPending,
			wantMsg:    `target "b": container "main" of Pod "restore-b" has no ID yet`,
		},
		{
			name:        "an agent that does not support pod groups",
			targetB:     pod(func(*corev1.Pod) {}),
			agents:      []client.Object{pgAgent("node-1", true, nil)},
			wantReason:  snapshotv1alpha1.PodGroupReasonAgentNotReady,
			wantMsg:     `waiting for the node agent on 1 node(s): node "node-1": agent Pod "agent-node-1" does not support pod groups`,
			wantRequeue: func(time.Duration) time.Duration { return podGroupAdmissionRequeue },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := append([]client.Object{rgSnapshot("a", "b"), rgRestore("a", "b"), rgTargetPod("a", "node-1")}, tc.targetB...)
			objs = append(append(objs, tc.extra...), tc.agents...)
			r, recorder := makePodGroupRestoreReconciler(t, objs...)

			res := reconcileRestore(t, r)

			got := getRestore(t, r)
			untilDeadline := time.Until(got.CreationTimestamp.Add(600 * time.Second))
			if tc.wantRequeue != nil {
				assert.Equal(t, tc.wantRequeue(untilDeadline), res.RequeueAfter, "polls agents")
			} else {
				assert.InDelta(t, untilDeadline.Seconds(), res.RequeueAfter.Seconds(), 5, "a target Pod's events wake it, not polling")
			}
			admitted := rgCondition(got.Status.Conditions, snapshotv1alpha1.PodGroupConditionAdmitted)
			require.NotNil(t, admitted)
			assert.Equal(t, metav1.ConditionFalse, admitted.Status)
			assert.Equal(t, tc.wantReason, admitted.Reason)
			assert.Equal(t, tc.wantMsg, admitted.Message)
			assert.Nil(t, rgCondition(got.Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed))
			for _, m := range got.Status.Members {
				assert.Equal(t, snapshotv1alpha1.RestorePhasePending, m.Phase, m.ID)
				assert.Nil(t, m.Pod, "nothing is pinned while admission waits")
			}
			assert.Empty(t, drainEvents(recorder))
		})
	}
}

func TestPodGroupRestoreReconciler_AFinishedRestoreReleasesItsPods(t *testing.T) {
	other := rgRestore("a")
	other.Name, other.UID = "other", "other-uid"
	other.Status = snapshotv1alpha1.PodGroupRestoreStatus{
		Members: []snapshotv1alpha1.PodGroupRestoreMemberStatus{{
			ID: "a", Phase: snapshotv1alpha1.RestorePhaseFailed, Reason: "Refused",
			Pod: &snapshotv1alpha1.ObjectIdentity{Name: "restore-a", UID: "restore-pod-uid-a"},
		}},
		Conditions: []metav1.Condition{{Type: snapshotv1alpha1.PodGroupConditionFailed, Status: metav1.ConditionTrue,
			Reason: "Refused", LastTransitionTime: metav1.Now()}},
	}
	r, _ := makePodGroupRestoreReconciler(t, append(rgReadyCluster("a"), rgRestore("a"), other)...)

	reconcileRestore(t, r)

	assert.Equal(t, snapshotv1alpha1.RestorePhaseAdmitted, getRestore(t, r).Status.Members[0].Phase)
}

func TestPodGroupRestoreReconciler_MemberReasons(t *testing.T) {
	setup := func(t *testing.T) *PodGroupRestoreReconciler {
		r, _ := makePodGroupRestoreReconciler(t, append(rgReadyCluster("a", "b"), rgRestore("a", "b"))...)
		reconcileRestore(t, r)
		rgReport(t, r, "b", prStarted)
		return r
	}

	t.Run("from GroupRestoreFailed", func(t *testing.T) {
		r := setup(t)
		rgReport(t, r, "a", prStarted, prRestoreError, prGroupFailed(snapshotv1alpha1.PodGroupReasonPodIPChanged))
		reconcileRestore(t, r)
		got := getRestore(t, r)
		assert.Equal(t, snapshotv1alpha1.PodGroupReasonPodIPChanged, got.Status.Members[0].Reason)
		assert.Equal(t, "PodIPChanged message", got.Status.Members[0].Message)
		assert.Equal(t, snapshotv1alpha1.PodGroupReasonPodIPChanged,
			rgCondition(got.Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed).Reason)
	})

	t.Run("from Restored when GroupRestoreFailed is absent", func(t *testing.T) {
		r := setup(t)
		rgReport(t, r, "a", prStarted, prRestoreError)
		reconcileRestore(t, r)
		got := getRestore(t, r)
		assert.Equal(t, podcontract.RestoreReasonFailed, got.Status.Members[0].Reason)
		assert.Equal(t, snapshotv1alpha1.PodGroupReasonMemberFailed,
			rgCondition(got.Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed).Reason)
	})

	t.Run("GroupRestoreFailed while Restored is not False fails nothing", func(t *testing.T) {
		r := setup(t)
		rgReport(t, r, "a", prStarted, prRestored(corev1.ConditionFalse, podcontract.RestoreReasonInProgress),
			prGroupFailed(snapshotv1alpha1.PodGroupReasonCoordinatorLost))
		reconcileRestore(t, r)
		got := getRestore(t, r)
		assert.Equal(t, snapshotv1alpha1.RestorePhaseRestoring, got.Status.Members[0].Phase)
		assert.Nil(t, rgCondition(got.Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed))
	})
}

func TestPodGroupRestoreReconciler_DeletedTargetPodFailsItsMember(t *testing.T) {
	r, recorder := makePodGroupRestoreReconciler(t, append(rgReadyCluster("a", "b"), rgRestore("a", "b"))...)
	reconcileRestore(t, r)
	ctx := context.Background()

	t.Run("a cache miss alone changes nothing", func(t *testing.T) {
		live := r.Client
		r.Client = interceptor.NewClient(live.(client.WithWatch), interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Pod); ok && key.Name == "restore-b" {
					return apierrors.NewNotFound(corev1.Resource("pods"), key.Name)
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
		defer func() { r.Client = live }()

		reconcileRestore(t, r)
		assert.Equal(t, snapshotv1alpha1.RestorePhaseAdmitted, getRestore(t, r).Status.Members[1].Phase)
	})

	t.Run("a deleted Pod", func(t *testing.T) {
		require.NoError(t, r.Delete(ctx, rgTargetPod("a", "")))

		res := reconcileRestore(t, r)

		got := getRestore(t, r)
		m := got.Status.Members[0]
		assert.Equal(t, snapshotv1alpha1.RestorePhaseFailed, m.Phase)
		assert.Equal(t, snapshotv1alpha1.PodGroupReasonTargetPodDeleted, m.Reason)
		assert.Equal(t, `Pod "restore-a" was deleted or replaced before the member was released`, m.Message)
		assert.Equal(t, types.UID("restore-pod-uid-a"), m.Pod.UID, "the pin is kept")
		assert.Nil(t, rgCondition(got.Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed),
			"member b's agent reports, or the deadline ends it")
		assert.Positive(t, res.RequeueAfter, "the deadline still applies")
		assert.Empty(t, drainEvents(recorder))
	})

	t.Run("a replacement Pod of the same name, which hides a start: MemberFailed, not Refused", func(t *testing.T) {
		require.NoError(t, r.Delete(ctx, rgTargetPod("b", "")))
		replacement := rgTargetPod("b", "node-1")
		replacement.UID = "replacement-uid"
		require.NoError(t, r.Create(ctx, replacement))

		reconcileRestore(t, r)

		got := getRestore(t, r)
		m := got.Status.Members[1]
		assert.Equal(t, snapshotv1alpha1.RestorePhaseFailed, m.Phase)
		assert.Equal(t, snapshotv1alpha1.PodGroupReasonTargetPodDeleted, m.Reason)
		failed := rgCondition(got.Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed)
		require.NotNil(t, failed)
		assert.Equal(t, snapshotv1alpha1.PodGroupReasonMemberFailed, failed.Reason)
		assert.Equal(t, `member "a" (rank 0) failed with TargetPodDeleted: `+
			`Pod "restore-a" was deleted or replaced before the member was released`, failed.Message)
	})
}

func TestPodGroupRestoreReconciler_Deletion(t *testing.T) {
	setup := func(t *testing.T) *PodGroupRestoreReconciler {
		r, _ := makePodGroupRestoreReconciler(t, append(rgReadyCluster("a", "b"), rgRestore("a", "b"))...)
		reconcileRestore(t, r)
		return r
	}
	deleteRestore := func(t *testing.T, r *PodGroupRestoreReconciler) {
		require.NoError(t, r.Delete(context.Background(), getRestore(t, r)))
	}

	t.Run("cancelled before any member starts", func(t *testing.T) {
		r := setup(t)
		var recorded *metav1.Condition
		r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				recorded = rgCondition(obj.(*snapshotv1alpha1.PodGroupRestore).Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed)
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		})
		deleteRestore(t, r)
		recorder := r.Recorder.(*record.FakeRecorder)

		reconcileRestore(t, r)

		require.NotNil(t, recorded, "Cancelled is recorded before the finalizer goes")
		assert.Equal(t, snapshotv1alpha1.PodGroupReasonCancelled, recorded.Reason)
		assert.True(t, restoreGone(t, r))
		assert.Equal(t, []string{"Warning Cancelled " + podGroupRestoreCancelledMessage}, drainEvents(recorder))
	})

	t.Run("held while a member is Restoring", func(t *testing.T) {
		r := setup(t)
		rgReport(t, r, "a", prStarted)
		deleteRestore(t, r)

		res := reconcileRestore(t, r)

		assert.False(t, restoreGone(t, r))
		assert.Nil(t, rgCondition(getRestore(t, r).Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed),
			"deletion does not fail a started restore")
		assert.Positive(t, res.RequeueAfter, "the deadline still applies")

		t.Run("still held once Failed while a member is Restoring", func(t *testing.T) {
			rgReport(t, r, "b", prRestoreError)
			reconcileRestore(t, r)
			got := getRestore(t, r)
			require.NotNil(t, rgCondition(got.Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed))
			assert.Equal(t, snapshotv1alpha1.RestorePhaseRestoring, got.Status.Members[0].Phase)
		})

		t.Run("released once no member is Restoring", func(t *testing.T) {
			rgReport(t, r, "a", prStarted, prRestoreError, prGroupFailed(snapshotv1alpha1.PodGroupReasonMemberFailed))
			reconcileRestore(t, r)
			assert.True(t, restoreGone(t, r))
		})
	})
}

func TestPodGroupRestoreRequeue(t *testing.T) {
	failed := metav1.Condition{Type: snapshotv1alpha1.PodGroupConditionFailed, Status: metav1.ConditionTrue, Reason: "MemberFailed"}
	waiting := func(reason string) podGroupAdmission {
		return podGroupAdmission{state: podGroupAdmissionWaiting, reason: reason}
	}
	for _, tc := range []struct {
		name       string
		admission  podGroupAdmission
		untilEnd   time.Duration
		conditions []metav1.Condition
		phases     []snapshotv1alpha1.RestorePhase
		want       time.Duration
	}{
		{"waits for an agent", waiting(snapshotv1alpha1.PodGroupReasonAgentNotReady), time.Hour, nil,
			[]snapshotv1alpha1.RestorePhase{"Pending"}, podGroupAdmissionRequeue},
		{"waits for an agent near the deadline", waiting(snapshotv1alpha1.PodGroupReasonAgentNotReady), 3 * time.Second, nil,
			[]snapshotv1alpha1.RestorePhase{"Pending"}, 3 * time.Second},
		{"waits for a target Pod", waiting(snapshotv1alpha1.PodGroupReasonTargetPodPending), time.Hour, nil,
			[]snapshotv1alpha1.RestorePhase{"Pending"}, time.Hour},
		{"a member restores", podGroupAdmission{}, time.Hour, nil, []snapshotv1alpha1.RestorePhase{"Restoring"}, time.Hour},
		{"failed while a member restores", podGroupAdmission{}, time.Hour, []metav1.Condition{failed},
			[]snapshotv1alpha1.RestorePhase{"Failed", "Restoring"}, time.Hour},
		{"ended", podGroupAdmission{}, time.Hour, nil, []snapshotv1alpha1.RestorePhase{"Released", "Failed"}, 0},
		{"past the deadline", podGroupAdmission{}, -time.Second, nil, []snapshotv1alpha1.RestorePhase{"Restoring"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := &snapshotv1alpha1.PodGroupRestoreStatus{Conditions: tc.conditions}
			for _, phase := range tc.phases {
				status.Members = append(status.Members, snapshotv1alpha1.PodGroupRestoreMemberStatus{Phase: phase})
			}
			observed := podGroupRestoreObservation{admission: tc.admission, deadline: pgNow.Add(tc.untilEnd)}
			assert.Equal(t, tc.want, podGroupRestoreRequeue(observed, status, pgNow))
		})
	}
}

func TestPodGroupRestoreMapsPods(t *testing.T) {
	pod := rgTargetPod("a", "node-1")
	assert.Equal(t, []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: pgNamespace, Name: "restore"}}},
		restoreGroupPodToPodGroupRestore(context.Background(), pod))
	pod.Annotations = nil
	assert.Empty(t, restoreGroupPodToPodGroupRestore(context.Background(), pod))
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
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

// makePodGroupRestoreReconciler builds a reconciler whose API server is one
// fake client, and whose cache serves the same objects as the operator's cache
// does (rgCache).
func makePodGroupRestoreReconciler(t *testing.T, objs ...client.Object) (*PodGroupRestoreReconciler, *record.FakeRecorder) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(snapshotReconcilerScheme()).WithObjects(objs...).
		WithStatusSubresource(&snapshotv1alpha1.PodGroupRestore{}, &snapshotv1alpha1.PodGroupSnapshot{}, &corev1.Pod{}).
		WithIndex(&snapshotv1alpha1.PodGroupRestore{}, podGroupRestoreTargetPodField, podGroupRestoreTargetPods).
		WithIndex(&snapshotv1alpha1.PodGroupRestore{}, podGroupRestoreSnapshotUIDField, podGroupRestoreSnapshotUID).
		Build()
	recorder := record.NewFakeRecorder(100)
	return &PodGroupRestoreReconciler{
		Client:             rgCache(c),
		NonCacheReadClient: c,
		Recorder:           recorder,
		AgentNamespace:     pgAgentNamespace,
		AgentSelector:      labels.SelectorFromSet(pgAgentLabels),
	}, recorder
}

// rgCache reads c's objects as the operator's cache serves them, every Pod
// trimmed by TrimPodForCache, and writes to c.
func rgCache(c client.WithWatch) client.WithWatch {
	return interceptor.NewClient(c, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			_, err := TrimPodForCache(obj)
			return err
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			if pods, ok := list.(*corev1.PodList); ok {
				for i := range pods.Items {
					if _, err := TrimPodForCache(&pods.Items[i]); err != nil {
						return err
					}
				}
			}
			return nil
		},
	})
}

// rgAPI is the reconciler's API server, through which a test reads and writes
// whole Pods.
func rgAPI(r *PodGroupRestoreReconciler) client.Client {
	return r.NonCacheReadClient.(client.Client)
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

// rgRestore is a PodGroupRestore named "restore", with UID "restore-uid", of
// rgSnapshot whose targets a, b, ... name the restore Pods restore-a,
// restore-b, ...; it already holds its finalizer.
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

// rgGatedPod is target id's restore Pod, restore-<id>, as its owner creates
// it: shaped by podcontract.BuildGroupTarget for destination container
// "main", gated, unbound, and not scheduled.
func rgGatedPod(id string) *corev1.Pod {
	pod, err := podcontract.BuildGroupTarget(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "restore-" + id, Namespace: pgNamespace, UID: types.UID("restore-pod-uid-" + id)},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "workload:latest"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}, podcontract.GroupTargetRequest{DestinationContainer: "main"}, podcontract.Options{})
	if err != nil {
		panic(err)
	}
	return pod
}

// rgClaim returns pod as the operator claims it for the PodGroupRestore with
// uid, mapping container "main" to itself.
func rgClaim(pod *corev1.Pod, uid types.UID) *corev1.Pod {
	claimed, err := podcontract.ClaimGroupTarget(pod, podcontract.RestoreGroupClaim{
		UID: uid, Mapping: podcontract.ContainerMapping{Source: "main", Destination: "main"},
	})
	if err != nil {
		panic(err)
	}
	return claimed
}

// rgRun schedules pod to node and runs it, as the scheduler and the kubelet
// do: with an address and a running destination container "main".
func rgRun(pod *corev1.Pod, node string) *corev1.Pod {
	id := strings.TrimPrefix(pod.Name, "restore-")
	pod.Spec.NodeName = node
	pod.Status = corev1.PodStatus{
		Phase:             corev1.PodRunning,
		PodIP:             "10.1.0." + strconv.Itoa(int(id[0])),
		ContainerStatuses: []corev1.ContainerStatus{{Name: "main", ContainerID: "containerd://" + id}},
	}
	return pod
}

// rgTargetPod is target id's restore Pod, claimed by "restore" and running on
// node.
func rgTargetPod(id, node string) *corev1.Pod {
	return rgRun(rgClaim(rgGatedPod(id), "restore-uid"), node)
}

// rgReadyCluster returns the snapshot of ids, their target Pods claimed and
// running on node-1, and a group-capable agent there.
func rgReadyCluster(ids ...string) []client.Object {
	objs := []client.Object{rgSnapshot(ids...), pgAgent("node-1", true, ptr.To("v1"))}
	for _, id := range ids {
		objs = append(objs, rgTargetPod(id, "node-1"))
	}
	return objs
}

// rgGatedCluster returns the snapshot of ids, their target Pods as their owner
// created them, and a group-capable agent on node-1.
func rgGatedCluster(ids ...string) []client.Object {
	objs := []client.Object{rgSnapshot(ids...), pgAgent("node-1", true, ptr.To("v1"))}
	for _, id := range ids {
		objs = append(objs, rgGatedPod(id))
	}
	return objs
}

func reconcileRestore(t *testing.T, r *PodGroupRestoreReconciler) ctrl.Result {
	t.Helper()
	return reconcileNamedRestore(t, r, "restore")
}

func reconcileNamedRestore(t *testing.T, r *PodGroupRestoreReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Namespace: pgNamespace, Name: name}})
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

// rgPod reads target id's whole Pod from the API server.
func rgPod(t *testing.T, r *PodGroupRestoreReconciler, id string) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{}
	require.NoError(t, rgAPI(r).Get(context.Background(), types.NamespacedName{Namespace: pgNamespace, Name: "restore-" + id}, pod))
	return pod
}

// rgWrite writes target id's Pod as change leaves it: its spec and metadata,
// then its status, as the scheduler and the kubelet would.
func rgWrite(t *testing.T, r *PodGroupRestoreReconciler, id string, change func(*corev1.Pod)) {
	t.Helper()
	ctx := context.Background()
	pod := rgPod(t, r, id)
	change(pod)
	status := pod.Status
	require.NoError(t, rgAPI(r).Update(ctx, pod))
	pod.Status = status
	require.NoError(t, rgAPI(r).Status().Update(ctx, pod))
}

// rgReport plays target id's agent: it writes conditions on the restore Pod,
// which its binding ties to the restore that claimed it.
func rgReport(t *testing.T, r *PodGroupRestoreReconciler, id string, conditions ...corev1.PodCondition) {
	t.Helper()
	pod := rgPod(t, r, id)
	pod.Status.Conditions = conditions
	require.NoError(t, rgAPI(r).Status().Update(context.Background(), pod))
}

// assertGated asserts that target id's Pod is still as its owner created it:
// gated and unbound.
func assertGated(t *testing.T, r *PodGroupRestoreReconciler, id string) {
	t.Helper()
	pod := rgPod(t, r, id)
	assert.True(t, podcontract.HasRestoreGroupGate(pod), "restore-%s keeps its gate", id)
	_, bound, _ := podcontract.RestoreGroupClaimOf(pod.Annotations)
	assert.False(t, bound, "restore-%s is unbound", id)
}

// assertClaimed asserts that target id's Pod was claimed for uid, and that
// the claim changed nothing else on before, the Pod as it was read for the
// claim.
func assertClaimed(t *testing.T, r *PodGroupRestoreReconciler, id string, uid types.UID, before *corev1.Pod) {
	t.Helper()
	want, got := rgClaim(before, uid), rgPod(t, r, id)
	want.ResourceVersion, got.ResourceVersion = "", ""
	assert.Equal(t, want, got, "restore-%s carries the claim and nothing else changed", id)
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

// rgCondition finds a condition.
func rgCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}

func TestPodGroupRestoreReconciler_AddsFinalizerFirst(t *testing.T) {
	restore := rgRestore("a")
	restore.Finalizers = nil
	r, _ := makePodGroupRestoreReconciler(t, append(rgGatedCluster("a"), restore)...)

	reconcileRestore(t, r)

	got := getRestore(t, r)
	assert.Equal(t, []string{snapshotv1alpha1.PodGroupInUseFinalizer}, got.Finalizers)
	assert.Empty(t, got.Status.Members, "the finalizer pass pins nothing")
	assertGated(t, r, "a")
}

// TestPodGroupRestoreReconciler_ClaimsEveryTargetOnceAllPass claims every
// target in one reconcile, each in one patch that changes nothing but the
// claim, and keeps another scheduler's gate; then waits for scheduling.
func TestPodGroupRestoreReconciler_ClaimsEveryTargetOnceAllPass(t *testing.T) {
	gang := corev1.PodSchedulingGate{Name: "example.com/gang"}
	b := rgGatedPod("b")
	b.Spec.SchedulingGates = append([]corev1.PodSchedulingGate{gang}, b.Spec.SchedulingGates...)
	r, recorder := makePodGroupRestoreReconciler(t, rgSnapshot("a", "b"), pgAgent("node-1", true, ptr.To("v1")),
		rgGatedPod("a"), b, rgRestore("a", "b"))
	before := map[string]*corev1.Pod{"a": rgPod(t, r, "a"), "b": rgPod(t, r, "b")}

	res := reconcileRestore(t, r)

	assertClaimed(t, r, "a", "restore-uid", before["a"])
	assertClaimed(t, r, "b", "restore-uid", before["b"])
	assert.Equal(t, "main=main", rgPod(t, r, "a").Annotations[podcontract.RestoreContainerMapAnnotation])
	assert.Equal(t, []corev1.PodSchedulingGate{gang}, rgPod(t, r, "b").Spec.SchedulingGates, "another scheduler's gate stays")
	got := getRestore(t, r)
	for _, m := range got.Status.Members {
		assert.Equal(t, snapshotv1alpha1.RestorePhasePending, m.Phase, m.ID)
		assert.Nil(t, m.Pod, "a claim pins nothing")
	}
	assert.Nil(t, rgCondition(got.Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed))
	assert.Positive(t, res.RequeueAfter)
	assert.Empty(t, drainEvents(recorder))

	t.Run("then waits for the claimed Pods to be scheduled", func(t *testing.T) {
		reconcileRestore(t, r)
		admitted := rgCondition(getRestore(t, r).Status.Conditions, snapshotv1alpha1.PodGroupConditionAdmitted)
		require.NotNil(t, admitted)
		assert.Equal(t, snapshotv1alpha1.PodGroupReasonTargetPodUnscheduled, admitted.Reason)
		assert.Equal(t, `target "a": Pod "restore-a" is not scheduled to a node yet; `+
			`target "b": Pod "restore-b" is not scheduled to a node yet`, admitted.Message)
	})
}

// TestPodGroupRestoreReconciler_ClaimsNothingUntilEveryTargetPasses keeps
// every target gated while a peer is missing or refused.
func TestPodGroupRestoreReconciler_ClaimsNothingUntilEveryTargetPasses(t *testing.T) {
	t.Run("a missing target", func(t *testing.T) {
		r, _ := makePodGroupRestoreReconciler(t, rgSnapshot("a", "b"), pgAgent("node-1", true, ptr.To("v1")),
			rgGatedPod("a"), rgRestore("a", "b"))

		res := reconcileRestore(t, r)

		admitted := rgCondition(getRestore(t, r).Status.Conditions, snapshotv1alpha1.PodGroupConditionAdmitted)
		require.NotNil(t, admitted)
		assert.Equal(t, snapshotv1alpha1.PodGroupReasonTargetPodPending, admitted.Reason)
		assert.Equal(t, `target "b": Pod "restore-b" does not exist yet`, admitted.Message)
		assert.InDelta(t, time.Until(getRestore(t, r).CreationTimestamp.Add(600*time.Second)).Seconds(), res.RequeueAfter.Seconds(), 5,
			"the Pod's creation wakes it, not polling")
		assertGated(t, r, "a")

		require.NoError(t, rgAPI(r).Create(context.Background(), rgGatedPod("b")))
		reconcileRestore(t, r)
		_, bound, _ := podcontract.RestoreGroupClaimOf(rgPod(t, r, "a").Annotations)
		assert.True(t, bound, "both are claimed once both exist")
	})

	t.Run("a refused target", func(t *testing.T) {
		ungated := rgGatedPod("b")
		ungated.Spec.SchedulingGates = nil
		r, _ := makePodGroupRestoreReconciler(t, rgSnapshot("a", "b"), rgGatedPod("a"), ungated, rgRestore("a", "b"))

		reconcileRestore(t, r)

		failed := rgCondition(getRestore(t, r).Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed)
		require.NotNil(t, failed)
		assert.Equal(t, snapshotv1alpha1.PodGroupReasonRefused, failed.Reason)
		assertGated(t, r, "a")
	})
}

// TestPodGroupRestoreReconciler_ClaimsAgainAfterAConflict makes the scheduler
// write target b's status between admission's read and the claim, as it does
// for a gated Pod. The claim's patch carries the resourceVersion admission
// read, so it conflicts; the next reconcile reads b again and claims it,
// keeping the scheduler's condition.
func TestPodGroupRestoreReconciler_ClaimsAgainAfterAConflict(t *testing.T) {
	r, _ := makePodGroupRestoreReconciler(t, append(rgGatedCluster("a", "b"), rgRestore("a", "b"))...)
	before := rgPod(t, r, "a")
	gatedCondition := corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "SchedulingGated"}
	written := false
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if pod, ok := obj.(*corev1.Pod); ok && pod.Name == "restore-b" && !written {
				written = true
				scheduler := rgPod(t, r, "b")
				scheduler.Status.Conditions = append(scheduler.Status.Conditions, gatedCondition)
				require.NoError(t, rgAPI(r).Status().Update(ctx, scheduler))
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: pgNamespace, Name: "restore"}})

	require.True(t, apierrors.IsConflict(err), "the reconcile ends with the conflict: %v", err)
	assertClaimed(t, r, "a", "restore-uid", before)
	assertGated(t, r, "b")
	assert.Nil(t, rgCondition(getRestore(t, r).Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed),
		"a conflict alone proves nothing")

	beforeB := rgPod(t, r, "b")
	reconcileRestore(t, r)
	assertClaimed(t, r, "b", "restore-uid", beforeB)
	assert.Contains(t, rgPod(t, r, "b").Status.Conditions, gatedCondition)
}

// TestPodGroupRestoreReconciler_ClaimsTargetsTheCacheHasNotSeen: both target
// Pods were just created, so the cache does not hold them yet. The claim stage
// reads the API server, which has them, and claims both in this reconcile
// rather than waiting with TargetPodPending for the cache.
func TestPodGroupRestoreReconciler_ClaimsTargetsTheCacheHasNotSeen(t *testing.T) {
	cached := fake.NewClientBuilder().WithScheme(snapshotReconcilerScheme()).
		WithObjects(pgObjects([]client.Object{rgSnapshot("a", "b"), rgRestore("a", "b")})...).Build()
	r, _ := makePodGroupRestoreReconciler(t, pgObjects(rgGatedCluster("a", "b"), []client.Object{rgRestore("a", "b")})...)
	before := map[string]*corev1.Pod{"a": rgPod(t, r, "a"), "b": rgPod(t, r, "b")}
	// Reads come from the lagging cache; writes reach the API server.
	r.Client = interceptor.NewClient(r.NonCacheReadClient.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, _ client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			return cached.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, _ client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			return cached.List(ctx, list, opts...)
		},
	})

	reconcileRestore(t, r)

	assertClaimed(t, r, "a", "restore-uid", before["a"])
	assertClaimed(t, r, "b", "restore-uid", before["b"])
}

// TestPodGroupRestoreReconciler_ResumesAfterACrashBetweenClaims finds target
// a claimed and b still gated, as a crash between the two claims leaves them,
// and claims b alone.
func TestPodGroupRestoreReconciler_ResumesAfterACrashBetweenClaims(t *testing.T) {
	r, _ := makePodGroupRestoreReconciler(t, rgSnapshot("a", "b"), pgAgent("node-1", true, ptr.To("v1")),
		rgClaim(rgGatedPod("a"), "restore-uid"), rgGatedPod("b"), rgRestore("a", "b"))
	a, b := rgPod(t, r, "a"), rgPod(t, r, "b")

	reconcileRestore(t, r)

	assert.Equal(t, a.ResourceVersion, rgPod(t, r, "a").ResourceVersion, "the claimed Pod is not written again")
	assertClaimed(t, r, "b", "restore-uid", b)
	assert.Nil(t, rgCondition(getRestore(t, r).Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed))
}

// TestPodGroupRestoreReconciler_TwoRestoresNameTheSamePod lets one restore
// claim a Pod, and refuses the other with TargetInUse, without reading it.
func TestPodGroupRestoreReconciler_TwoRestoresNameTheSamePod(t *testing.T) {
	other := rgRestore("a")
	other.Name, other.UID = "other", "other-uid"
	r, recorder := makePodGroupRestoreReconciler(t, append(rgGatedCluster("a"), other, rgRestore("a"))...)
	before := rgPod(t, r, "a")

	reconcileNamedRestore(t, r, "other")
	reconcileRestore(t, r)

	assertClaimed(t, r, "a", "other-uid", before)
	failed := rgCondition(getRestore(t, r).Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, snapshotv1alpha1.PodGroupReasonTargetInUse, failed.Reason)
	assert.Equal(t, `target "a": Pod "restore-a" is claimed by another PodGroupRestore, with UID "other-uid"`, failed.Message)
	assert.Equal(t, []string{"Warning TargetInUse " + failed.Message}, drainEvents(recorder))
}

// TestPodGroupRestoreReconciler_PinsOnceEveryClaimedPodRuns follows claimed
// Pods through scheduling, one of them held by a gang's gate, to the pins:
// TargetPodUnscheduled, then TargetPodPending, then one status patch.
func TestPodGroupRestoreReconciler_PinsOnceEveryClaimedPodRuns(t *testing.T) {
	gang := corev1.PodSchedulingGate{Name: "example.com/gang"}
	b := rgClaim(rgGatedPod("b"), "restore-uid")
	b.Spec.SchedulingGates = []corev1.PodSchedulingGate{gang}
	r, _ := makePodGroupRestoreReconciler(t, rgSnapshot("a", "b"), pgAgent("node-1", true, ptr.To("v1")),
		pgAgent("node-2", true, ptr.To("v1")), rgClaim(rgGatedPod("a"), "restore-uid"), b, rgRestore("a", "b"))
	admitted := func() *metav1.Condition {
		t.Helper()
		condition := rgCondition(getRestore(t, r).Status.Conditions, snapshotv1alpha1.PodGroupConditionAdmitted)
		require.NotNil(t, condition)
		return condition
	}
	scheduled := func(node string) func(*corev1.Pod) {
		return func(pod *corev1.Pod) { pod.Spec.SchedulingGates, pod.Spec.NodeName = nil, node }
	}

	res := reconcileRestore(t, r)
	assert.Equal(t, snapshotv1alpha1.PodGroupReasonTargetPodUnscheduled, admitted().Reason)
	assert.InDelta(t, time.Until(getRestore(t, r).CreationTimestamp.Add(600*time.Second)).Seconds(), res.RequeueAfter.Seconds(), 5,
		"a Pod's scheduling wakes it, not polling")

	rgWrite(t, r, "a", scheduled("node-1"))
	reconcileRestore(t, r)
	assert.Equal(t, `target "b": Pod "restore-b" is not scheduled to a node yet`, admitted().Message,
		"the gang's gate still holds b")

	rgWrite(t, r, "b", scheduled("node-2"))
	reconcileRestore(t, r)
	assert.Equal(t, snapshotv1alpha1.PodGroupReasonTargetPodPending, admitted().Reason)
	assert.Equal(t, `target "a": Pod "restore-a" has no IP address yet; target "b": Pod "restore-b" has no IP address yet`,
		admitted().Message)

	rgWrite(t, r, "a", func(pod *corev1.Pod) { rgRun(pod, "node-1") })
	rgWrite(t, r, "b", func(pod *corev1.Pod) { rgRun(pod, "node-2") })
	patches := countStatusPatches(r)
	reconcileRestore(t, r)
	assert.Equal(t, 1, *patches, "every member is pinned in one status patch")
	got := getRestore(t, r)
	assert.Equal(t, snapshotv1alpha1.PodGroupReasonAdmitting, admitted().Reason)
	for i, node := range []string{"node-1", "node-2"} {
		m := got.Status.Members[i]
		assert.Equal(t, snapshotv1alpha1.RestorePhaseAdmitted, m.Phase, m.ID)
		assert.Equal(t, node, m.NodeName, m.ID)
		assert.Equal(t, rgPod(t, r, m.ID).UID, m.Pod.UID, m.ID)
	}
}

// TestPodGroupRestoreReconciler_Lifecycle pins two members on different nodes,
// targets listed out of rank order, in one patch, then follows their agents'
// reports to Ready, checking at every step that a freshly started reconciler
// derives the same status and emits nothing.
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

// onPodB returns a change of the cluster that changes target b's Pod.
func onPodB(change func(pod *corev1.Pod)) func(objs []client.Object) {
	return func(objs []client.Object) {
		for _, obj := range objs {
			if pod, ok := obj.(*corev1.Pod); ok && pod.Name == "restore-b" {
				change(pod)
			}
		}
	}
}

// rgRefusalCase is a restore of targets a and b that admission refuses.
type rgRefusalCase struct {
	name       string
	restore    func(restore *snapshotv1alpha1.PodGroupRestore)
	cluster    func(objs []client.Object)
	wantReason string
	wantMsg    string
	// wantPrefix, when set, replaces wantMsg with a prefix of the message.
	wantPrefix string
}

// runRefusals runs each refusal case on the restore of a and b and the given
// Pods, with no agent anywhere, since a refusal never waits for one. Each is
// refused with nothing pinned and one warning event, leaves every Pod as it
// was, and stays as it is on the next pass.
func runRefusals(t *testing.T, pods func() []client.Object, cases []rgRefusalCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restore := rgRestore("a", "b")
			if tc.restore != nil {
				tc.restore(restore)
			}
			objs := append([]client.Object{rgSnapshot("a", "b")}, pods()...)
			if tc.cluster != nil {
				tc.cluster(objs)
			}
			r, recorder := makePodGroupRestoreReconciler(t, append(objs, restore)...)
			before := map[string]*corev1.Pod{"a": rgPod(t, r, "a"), "b": rgPod(t, r, "b")}

			res := reconcileRestore(t, r)

			assert.Zero(t, res.RequeueAfter)
			got := getRestore(t, r)
			failed := rgCondition(got.Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed)
			require.NotNil(t, failed)
			assert.Equal(t, tc.wantReason, failed.Reason)
			if tc.wantPrefix != "" {
				assert.True(t, strings.HasPrefix(failed.Message, tc.wantPrefix), failed.Message)
			} else {
				assert.Equal(t, tc.wantMsg, failed.Message)
			}
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
			for id, pod := range before {
				assert.Equal(t, pod, rgPod(t, r, id), "restore-%s is left as it was: never claimed, never repaired", id)
			}
			events := drainEvents(recorder)
			require.Len(t, events, 1)
			assert.True(t, strings.HasPrefix(events[0], "Warning "+tc.wantReason+" "), events[0])

			// A refusal is final: nothing changes on the next pass.
			reconcileRestore(t, r)
			assert.Equal(t, got.ResourceVersion, getRestore(t, r).ResourceVersion)
		})
	}
}

func TestPodGroupRestoreReconciler_RefusesBeforeClaimingAnything(t *testing.T) {
	gatedPods := func() []client.Object { return []client.Object{rgGatedPod("a"), rgGatedPod("b")} }
	const claimRefusal = `target "b": Pod "restore-b" cannot be claimed: `
	runRefusals(t, gatedPods, []rgRefusalCase{
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
			name:       "the snapshot is being deleted",
			cluster:    func(objs []client.Object) { objs[0].SetDeletionTimestamp(ptr.To(metav1.Now())) },
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
			name:       "a Pod created without the gate",
			cluster:    onPodB(func(pod *corev1.Pod) { pod.Spec.SchedulingGates = nil }),
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantMsg: `target "b": Pod "restore-b" is not a group target: ` +
				`it has neither the nvidia.com/restore-group scheduling gate nor a claim`,
		},
		{
			name: "a Pod another restore claimed",
			cluster: onPodB(func(pod *corev1.Pod) {
				*pod = *rgClaim(pod, "other-uid")
			}),
			wantReason: snapshotv1alpha1.PodGroupReasonTargetInUse,
			wantMsg:    `target "b": Pod "restore-b" is claimed by another PodGroupRestore, with UID "other-uid"`,
		},
		{
			name: "a gated Pod whose binding something else wrote",
			cluster: onPodB(func(pod *corev1.Pod) {
				pod.Annotations = map[string]string{
					podcontract.RestoreGroupUIDAnnotation:     "restore-uid",
					podcontract.RestoreContainerMapAnnotation: "main=main",
				}
			}),
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantMsg: `target "b": Pod "restore-b" is bound to this PodGroupRestore but still carries the ` +
				`nvidia.com/restore-group scheduling gate: something other than Snapshot wrote its binding`,
		},
		{
			name:       "a stale UID",
			restore:    func(r *snapshotv1alpha1.PodGroupRestore) { r.Spec.Targets[1].PodRef.UID = "old-pod-uid" },
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantMsg:    `target "b": Pod "restore-b" has UID "restore-pod-uid-b", not "old-pod-uid": it was recreated`,
		},
		{
			name: "a destination that is not shaped as one",
			cluster: onPodB(func(pod *corev1.Pod) {
				pod.Spec.Containers[0].StartupProbe = nil
			}),
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantPrefix: claimRefusal + "the claim's destination is not a group target's: ",
		},
		{
			name: "a mapping into a container the Pod lacks",
			restore: func(r *snapshotv1alpha1.PodGroupRestore) {
				r.Spec.Targets[1].ContainerMappings = []snapshotv1alpha1.ContainerMapping{{Source: "main", Destination: "server"}}
			},
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantMsg: claimRefusal + `the claim's destination is not a group target's: ` +
				`restore pod has no destination container named "server"`,
		},
		{
			name: "a gated Pod whose owner wrote a container map",
			cluster: onPodB(func(pod *corev1.Pod) {
				pod.Annotations = map[string]string{podcontract.RestoreContainerMapAnnotation: "main=main"}
			}),
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantMsg:    claimRefusal + "a group target to claim must not carry nvidia.com/restore-container-map",
		},
	})
}

func TestPodGroupRestoreReconciler_RefusesAfterTheClaim(t *testing.T) {
	claimedPods := func() []client.Object { return []client.Object{rgTargetPod("a", "node-1"), rgTargetPod("b", "node-1")} }
	runRefusals(t, claimedPods, []rgRefusalCase{
		{
			name:       "a claimed Pod whose UID is not the one the spec names",
			restore:    func(r *snapshotv1alpha1.PodGroupRestore) { r.Spec.Targets[1].PodRef.UID = "old-pod-uid" },
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantMsg:    `target "b": Pod "restore-b" has UID "restore-pod-uid-b", not "old-pod-uid": it was recreated`,
		},
		{
			name:       "an IPv6 address",
			cluster:    onPodB(func(pod *corev1.Pod) { pod.Status.PodIP = "fd00::2" }),
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantMsg:    `target "b": Pod "restore-b" has the address "fd00::2", but a group restore supports only IPv4`,
		},
		{
			name: "a claimed Pod whose container map something else changed",
			cluster: onPodB(func(pod *corev1.Pod) {
				pod.Annotations[podcontract.RestoreContainerMapAnnotation] = "main=sidecar"
			}),
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantMsg:    `target "b": Pod "restore-b" is claimed by this PodGroupRestore with the container map main=sidecar, not main=main`,
		},
		{
			name: "Pods claimed by an earlier PodGroupRestore of the same name",
			cluster: func(objs []client.Object) {
				for _, obj := range objs {
					if pod, ok := obj.(*corev1.Pod); ok {
						pod.Annotations[podcontract.RestoreGroupUIDAnnotation] = "earlier-restore-uid"
					}
				}
			},
			wantReason: snapshotv1alpha1.PodGroupReasonTargetInUse,
			wantMsg:    `target "a": Pod "restore-a" is claimed by another PodGroupRestore, with UID "earlier-restore-uid"`,
		},
	})
}

// makeLaggingPodGroupRestoreReconciler returns a reconciler whose cache serves
// cached, as rgCache serves it, while the API server holds live: the cache
// lags behind the API server. Status is recorded in the cache's client.
func makeLaggingPodGroupRestoreReconciler(t *testing.T, cached, live []client.Object) (*PodGroupRestoreReconciler, *record.FakeRecorder) {
	t.Helper()
	r, recorder := makePodGroupRestoreReconciler(t, cached...)
	api, _ := makePodGroupRestoreReconciler(t, live...)
	r.NonCacheReadClient = api.NonCacheReadClient
	return r, recorder
}

// rgPinnedMember is target id's member status as an earlier reconcile pinned
// it to rgTargetPod(id, "node-1"), in phase.
func rgPinnedMember(id string, phase snapshotv1alpha1.RestorePhase) snapshotv1alpha1.PodGroupRestoreMemberStatus {
	pod := rgTargetPod(id, "node-1")
	m := snapshotv1alpha1.PodGroupRestoreMemberStatus{
		ID: id, Phase: phase,
		Pod:         &snapshotv1alpha1.ObjectIdentity{Name: pod.Name, UID: pod.UID},
		NodeName:    "node-1",
		Containers:  []snapshotv1alpha1.PodGroupRestoreContainerStatus{{Name: "main", ContainerID: "containerd://" + id}},
		TargetPodIP: pod.Status.PodIP,
	}
	if phase == snapshotv1alpha1.RestorePhaseRestoring {
		m.StartedAt = pgStartedAt.DeepCopy()
	}
	return m
}

// rgCommittedRestore is "restore" with members, as earlier reconciles recorded
// them, committed, and created age ago.
func rgCommittedRestore(age time.Duration, members ...snapshotv1alpha1.PodGroupRestoreMemberStatus) *snapshotv1alpha1.PodGroupRestore {
	ids := make([]string, len(members))
	for i, m := range members {
		ids[i] = m.ID
	}
	restore := rgRestore(ids...)
	restore.CreationTimestamp = metav1.NewTime(time.Now().Add(-age).Truncate(time.Second))
	restore.Status.Members = members
	restore.Status.Conditions = []metav1.Condition{{
		Type: snapshotv1alpha1.PodGroupConditionAdmitted, Status: metav1.ConditionTrue,
		Reason: snapshotv1alpha1.PodGroupReasonCommitted, Message: podGroupRestoreKind.committedMessage,
		LastTransitionTime: metav1.NewTime(restore.CreationTimestamp.Add(time.Second)),
	}}
	return restore
}

// rgReported is target id's running Pod carrying conditions.
func rgReported(id string, conditions ...corev1.PodCondition) *corev1.Pod {
	pod := rgTargetPod(id, "node-1")
	pod.Status.Conditions = conditions
	return pod
}

// TestPodGroupRestoreReconciler_ConfirmsTheDeadline: at the deadline, member
// a is Released on the API server, while the cache misses its Pod or holds a
// copy from before the release, also when the restore already failed because
// of member b. The status is derived again from the persisted one, so the
// cached pass's TargetPodDeleted or DeadlineExceeded for a does not stick.
func TestPodGroupRestoreReconciler_ConfirmsTheDeadline(t *testing.T) {
	firstCause := metav1.Condition{
		Type: snapshotv1alpha1.PodGroupConditionFailed, Status: metav1.ConditionTrue,
		Reason: snapshotv1alpha1.PodGroupReasonMemberFailed, Message: "the first cause", LastTransitionTime: metav1.Now(),
	}
	failedB := rgPinnedMember("b", snapshotv1alpha1.RestorePhaseRestoring)
	failedB.Phase, failedB.Reason, failedB.Message = snapshotv1alpha1.RestorePhaseFailed, podcontract.RestoreReasonFailed, "b failed"
	for _, tc := range []struct {
		name      string
		restore   func() *snapshotv1alpha1.PodGroupRestore
		cachedPod []client.Object
		podB      corev1.PodCondition
	}{
		{
			name: "the cache misses the Pod",
			restore: func() *snapshotv1alpha1.PodGroupRestore {
				return rgCommittedRestore(time.Hour, rgPinnedMember("a", snapshotv1alpha1.RestorePhaseRestoring),
					rgPinnedMember("b", snapshotv1alpha1.RestorePhaseRestoring))
			},
		},
		{
			name: "the cache holds a stale copy",
			restore: func() *snapshotv1alpha1.PodGroupRestore {
				return rgCommittedRestore(time.Hour, rgPinnedMember("a", snapshotv1alpha1.RestorePhaseRestoring),
					rgPinnedMember("b", snapshotv1alpha1.RestorePhaseRestoring))
			},
			cachedPod: []client.Object{rgReported("a", prStarted)},
		},
		{
			name: "the restore already failed because of another member",
			restore: func() *snapshotv1alpha1.PodGroupRestore {
				restore := rgCommittedRestore(time.Hour, rgPinnedMember("a", snapshotv1alpha1.RestorePhaseRestoring), failedB)
				restore.Status.Conditions = append(restore.Status.Conditions, firstCause)
				return restore
			},
			cachedPod: []client.Object{rgReported("a", prStarted)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			podB := rgReported("b", prStarted)
			r, _ := makeLaggingPodGroupRestoreReconciler(t,
				pgObjects([]client.Object{rgSnapshot("a", "b"), tc.restore(), podB}, tc.cachedPod),
				pgObjects([]client.Object{rgSnapshot("a", "b"), tc.restore(), podB, rgReported("a", prStarted, prReleased)}),
			)

			reconcileRestore(t, r)

			got := getRestore(t, r)
			assert.Equal(t, snapshotv1alpha1.RestorePhaseReleased, got.Status.Members[0].Phase, "not failed from the stale view")
			assert.Equal(t, snapshotv1alpha1.RestorePhaseFailed, got.Status.Members[1].Phase)
			failed := rgCondition(got.Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed)
			require.NotNil(t, failed)
			assert.Equal(t, metav1.ConditionTrue, failed.Status)
		})
	}
}

// TestPodGroupRestoreReconciler_ConfirmsACancellation deletes a restore whose
// member a started on the API server only. The cached pass would cancel it
// and release its finalizer; the API server's pass keeps both.
func TestPodGroupRestoreReconciler_ConfirmsACancellation(t *testing.T) {
	restore := func() *snapshotv1alpha1.PodGroupRestore {
		restore := rgRestore("a", "b")
		restore.DeletionTimestamp = ptr.To(metav1.Now())
		restore.Status.Members = []snapshotv1alpha1.PodGroupRestoreMemberStatus{
			rgPinnedMember("a", snapshotv1alpha1.RestorePhaseAdmitted), rgPinnedMember("b", snapshotv1alpha1.RestorePhaseAdmitted),
		}
		return restore
	}
	r, recorder := makeLaggingPodGroupRestoreReconciler(t,
		pgObjects([]client.Object{rgSnapshot("a", "b"), restore(), rgReported("a"), rgReported("b")}),
		pgObjects([]client.Object{rgSnapshot("a", "b"), restore(), rgReported("a", prStarted), rgReported("b")}),
	)

	reconcileRestore(t, r)

	require.False(t, restoreGone(t, r), "the finalizer stays")
	got := getRestore(t, r)
	assert.Contains(t, got.Finalizers, snapshotv1alpha1.PodGroupInUseFinalizer)
	assert.Nil(t, rgCondition(got.Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed), "not cancelled")
	assert.Equal(t, snapshotv1alpha1.RestorePhaseRestoring, got.Status.Members[0].Phase)
	assert.Equal(t, []string{"Normal Committed every member passed admission, and the restore has started"}, drainEvents(recorder))
}

// TestPodGroupRestoreReconciler_RecordsNothingWhenTheLiveReadFails: at the
// deadline, the API server's pass fails, so nothing is recorded.
func TestPodGroupRestoreReconciler_RecordsNothingWhenTheLiveReadFails(t *testing.T) {
	restore := rgCommittedRestore(time.Hour, rgPinnedMember("a", snapshotv1alpha1.RestorePhaseRestoring))
	r, recorder := makePodGroupRestoreReconciler(t, rgSnapshot("a"), restore, rgReported("a", prStarted))
	r.NonCacheReadClient = interceptor.NewClient(r.NonCacheReadClient.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Pod); ok {
				return apierrors.NewInternalError(errors.New("the API server is down"))
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	before := getRestore(t, r)

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: pgNamespace, Name: "restore"}})

	require.Error(t, err)
	assert.True(t, apierrors.IsInternalError(err), "the live read's error is returned: %v", err)
	after := getRestore(t, r)
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "nothing is recorded")
	assert.Empty(t, drainEvents(recorder))
}

// TestPodGroupRestoreReconciler_WakesOnItsSnapshot: a restore that waits for
// a target Pod does not poll, so its snapshot's events must reconcile it. It
// is refused as soon as its snapshot's deletion starts, or the snapshot loses
// Ready; a snapshot being deleted is then released.
func TestPodGroupRestoreReconciler_WakesOnItsSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name       string
		change     func(t *testing.T, r *PodGroupRestoreReconciler, snapshot *snapshotv1alpha1.PodGroupSnapshot)
		wantReason string
		wantMsg    string
		released   bool
	}{
		{
			name: "its snapshot's deletion starts",
			change: func(t *testing.T, r *PodGroupRestoreReconciler, snapshot *snapshotv1alpha1.PodGroupSnapshot) {
				require.NoError(t, r.Delete(context.Background(), snapshot))
			},
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantMsg:    `PodGroupSnapshot "group" is being deleted`,
			released:   true,
		},
		{
			name: "its snapshot loses Ready",
			change: func(t *testing.T, r *PodGroupRestoreReconciler, snapshot *snapshotv1alpha1.PodGroupSnapshot) {
				snapshot.Status.Conditions = []metav1.Condition{{Type: snapshotv1alpha1.PodGroupConditionReady,
					Status: metav1.ConditionFalse, Reason: snapshotv1alpha1.PodGroupReasonArtifactUnavailable,
					Message: "its PodSnapshotContent is gone", LastTransitionTime: metav1.Now()}}
				require.NoError(t, r.Status().Update(context.Background(), snapshot))
			},
			wantReason: snapshotv1alpha1.PodGroupReasonArtifactUnavailable,
			wantMsg:    `PodGroupSnapshot "group" is not Ready: its PodSnapshotContent is gone`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r, _ := makePodGroupRestoreReconciler(t, rgSnapshot("a", "b"), rgGatedPod("a"), rgRestore("a", "b"))
			snapshots := &PodGroupSnapshotReconciler{Client: r.Client, NonCacheReadClient: r.NonCacheReadClient,
				Recorder: record.NewFakeRecorder(10), AgentNamespace: pgAgentNamespace, AgentSelector: r.AgentSelector}
			reconcileRestore(t, r)
			require.Equal(t, snapshotv1alpha1.PodGroupReasonTargetPodPending,
				rgCondition(getRestore(t, r).Status.Conditions, snapshotv1alpha1.PodGroupConditionAdmitted).Reason)
			snapshot := &snapshotv1alpha1.PodGroupSnapshot{}
			require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: pgNamespace, Name: "group"}, snapshot))

			tc.change(t, r, snapshot)
			require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: pgNamespace, Name: "group"}, snapshot))
			requests := r.podGroupSnapshotToPodGroupRestores(ctx, snapshot)
			require.Equal(t, []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: pgNamespace, Name: "restore"}}},
				requests, "the snapshot's event reconciles the restore")
			reconcileRestore(t, r)

			failed := rgCondition(getRestore(t, r).Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed)
			require.NotNil(t, failed)
			assert.Equal(t, tc.wantReason, failed.Reason)
			assert.Equal(t, tc.wantMsg, failed.Message)
			assertGated(t, r, "a")
			if tc.released {
				_, err := snapshots.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: pgNamespace, Name: "group"}})
				require.NoError(t, err)
				err = r.Get(ctx, types.NamespacedName{Namespace: pgNamespace, Name: "group"}, &snapshotv1alpha1.PodGroupSnapshot{})
				assert.True(t, apierrors.IsNotFound(err), "the refused restore no longer holds its snapshot: %v", err)
			}
		})
	}
}

// TestPodGroupRestoreReconciler_ConfirmsARefusal sees target b with an IPv6
// address in the cache only: the API server has its IPv4 address, so the
// restore is pinned.
func TestPodGroupRestoreReconciler_ConfirmsARefusal(t *testing.T) {
	r, _ := makePodGroupRestoreReconciler(t, append(rgReadyCluster("a", "b"), rgRestore("a", "b"))...)
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if pod, ok := obj.(*corev1.Pod); ok && pod.Name == "restore-b" {
				pod.Status.PodIP = "fd00::2"
			}
			return nil
		},
	})

	reconcileRestore(t, r)

	got := getRestore(t, r)
	assert.Nil(t, rgCondition(got.Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed), "the API server has an IPv4 address")
	assert.Equal(t, snapshotv1alpha1.RestorePhaseAdmitted, got.Status.Members[1].Phase)
	assert.Equal(t, rgTargetPod("b", "").Status.PodIP, got.Status.Members[1].TargetPodIP)
}

// TestPodGroupRestoreReconciler_ConfirmsAPass sees target b claimed with this
// restore's map in the cache only: the API server shows the map that
// something else wrote since, so the restore is refused with nothing pinned.
func TestPodGroupRestoreReconciler_ConfirmsAPass(t *testing.T) {
	r, recorder := makePodGroupRestoreReconciler(t, append(rgReadyCluster("a", "b"), rgRestore("a", "b"))...)
	rgWrite(t, r, "b", func(pod *corev1.Pod) { pod.Annotations[podcontract.RestoreContainerMapAnnotation] = "main=sidecar" })
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if pod, ok := obj.(*corev1.Pod); ok && pod.Name == "restore-b" {
				pod.Annotations[podcontract.RestoreContainerMapAnnotation] = "main=main"
			}
			return nil
		},
	})

	reconcileRestore(t, r)

	got := getRestore(t, r)
	failed := rgCondition(got.Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, snapshotv1alpha1.PodGroupReasonRefused, failed.Reason)
	assert.Equal(t, `target "b": Pod "restore-b" is claimed by this PodGroupRestore with the container map main=sidecar, not main=main`,
		failed.Message)
	assert.Nil(t, got.Status.Members[0].Pod, "nothing is pinned")
	assert.Equal(t, []string{"Warning Refused " + failed.Message}, drainEvents(recorder))
}

func TestPodGroupRestoreReconciler_Waits(t *testing.T) {
	podB := func(mutate func(*corev1.Pod)) []client.Object {
		p := rgTargetPod("b", "node-1")
		mutate(p)
		return []client.Object{p}
	}
	groupAgent := []client.Object{pgAgent("node-1", true, ptr.To("v1"))}
	for _, tc := range []struct {
		name        string
		targetB     []client.Object
		agents      []client.Object
		wantReason  string
		wantMsg     string
		wantRequeue func(deadline time.Duration) time.Duration
	}{
		{
			name:       "a target Pod that does not exist yet",
			agents:     groupAgent,
			wantReason: snapshotv1alpha1.PodGroupReasonTargetPodPending,
			wantMsg:    `target "b": Pod "restore-b" does not exist yet`,
		},
		{
			name:       "a claimed target Pod that is not scheduled",
			targetB:    []client.Object{rgClaim(rgGatedPod("b"), "restore-uid")},
			agents:     groupAgent,
			wantReason: snapshotv1alpha1.PodGroupReasonTargetPodUnscheduled,
			wantMsg:    `target "b": Pod "restore-b" is not scheduled to a node yet`,
		},
		{
			name:       "a target Pod with no address yet",
			targetB:    podB(func(p *corev1.Pod) { p.Status = corev1.PodStatus{Phase: corev1.PodPending} }),
			agents:     groupAgent,
			wantReason: snapshotv1alpha1.PodGroupReasonTargetPodPending,
			wantMsg:    `target "b": Pod "restore-b" has no IP address yet`,
		},
		{
			name:       "a pending target Pod with no container status yet",
			targetB:    podB(func(p *corev1.Pod) { p.Status.Phase, p.Status.ContainerStatuses = corev1.PodPending, nil }),
			agents:     groupAgent,
			wantReason: snapshotv1alpha1.PodGroupReasonTargetPodPending,
			wantMsg:    `target "b": Pod "restore-b" has no ID yet for container "main"`,
		},
		{
			name:       "a destination container with no ID yet",
			targetB:    podB(func(p *corev1.Pod) { p.Status.ContainerStatuses[0].ContainerID = "" }),
			agents:     groupAgent,
			wantReason: snapshotv1alpha1.PodGroupReasonTargetPodPending,
			wantMsg:    `target "b": Pod "restore-b" has no ID yet for container "main"`,
		},
		{
			name:        "no agent on the claimed Pods' node",
			targetB:     podB(func(*corev1.Pod) {}),
			wantReason:  snapshotv1alpha1.PodGroupReasonAgentNotReady,
			wantMsg:     `waiting for the node agent on 1 node(s): node "node-1": no node agent Pod runs on it`,
			wantRequeue: func(time.Duration) time.Duration { return podGroupAdmissionRequeue },
		},
		{
			name:        "an agent that does not support pod groups",
			targetB:     podB(func(*corev1.Pod) {}),
			agents:      []client.Object{pgAgent("node-1", true, nil)},
			wantReason:  snapshotv1alpha1.PodGroupReasonAgentNotReady,
			wantMsg:     `waiting for the node agent on 1 node(s): node "node-1": agent Pod "agent-node-1" does not support pod groups`,
			wantRequeue: func(time.Duration) time.Duration { return podGroupAdmissionRequeue },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := append([]client.Object{rgSnapshot("a", "b"), rgRestore("a", "b"), rgTargetPod("a", "node-1")}, tc.targetB...)
			r, recorder := makePodGroupRestoreReconciler(t, append(objs, tc.agents...)...)

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
		cache := r.Client
		r.Client = interceptor.NewClient(cache.(client.WithWatch), interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Pod); ok && key.Name == "restore-b" {
					return apierrors.NewNotFound(corev1.Resource("pods"), key.Name)
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
		defer func() { r.Client = cache }()

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

	t.Run("a restore deleted before its claim claims nothing", func(t *testing.T) {
		r, _ := makePodGroupRestoreReconciler(t, append(rgGatedCluster("a", "b"), rgRestore("a", "b"))...)
		deleteRestore(t, r)

		reconcileRestore(t, r)

		assert.True(t, restoreGone(t, r))
		assertGated(t, r, "a")
		assertGated(t, r, "b")
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
		{"waits for a target Pod to be scheduled", waiting(snapshotv1alpha1.PodGroupReasonTargetPodUnscheduled), time.Hour, nil,
			[]snapshotv1alpha1.RestorePhase{"Pending"}, time.Hour},
		{"claimed its targets", podGroupAdmission{}, time.Hour, nil, []snapshotv1alpha1.RestorePhase{"Pending"}, time.Hour},
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

// TestPodGroupRestoreMapsPods maps a Pod to every restore in its namespace
// whose targets name it, whatever the Pod carries: a gated Pod names no
// restore.
func TestPodGroupRestoreMapsPods(t *testing.T) {
	other := rgRestore("a")
	other.Name, other.UID = "other", "other-uid"
	elsewhere := rgRestore("a", "b")
	elsewhere.Namespace = "elsewhere"
	r, _ := makePodGroupRestoreReconciler(t, rgRestore("a", "b"), other, elsewhere)
	requests := func(pod *corev1.Pod) []string {
		var names []string
		for _, request := range r.podToPodGroupRestores(context.Background(), pod) {
			names = append(names, request.Namespace+"/"+request.Name)
		}
		return names
	}

	assert.ElementsMatch(t, []string{pgNamespace + "/restore", pgNamespace + "/other"}, requests(rgGatedPod("a")))
	assert.Equal(t, []string{pgNamespace + "/restore"}, requests(rgTargetPod("b", "node-1")))
	assert.Empty(t, requests(rgGatedPod("c")))

	snapshot := rgSnapshot("a", "b")
	byUID := func(uid types.UID) []string {
		snapshot.UID = uid
		var names []string
		for _, request := range r.podGroupSnapshotToPodGroupRestores(context.Background(), snapshot) {
			names = append(names, request.Namespace+"/"+request.Name)
		}
		return names
	}
	assert.ElementsMatch(t, []string{pgNamespace + "/restore", pgNamespace + "/other"}, byUID("group-uid"),
		"a snapshot maps to the restores in its namespace that restore it")
	assert.Empty(t, byUID("recreated-group-uid"), "a snapshot recreated under the same name restores nothing")
}

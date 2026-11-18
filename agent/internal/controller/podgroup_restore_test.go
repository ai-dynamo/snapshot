// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	clientgotesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	"github.com/ai-dynamo/snapshot/agent/internal/group"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/targetstatus"
	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/coordinator"
	"github.com/ai-dynamo/snapshot/coordinator/coordinatortest"
	"github.com/ai-dynamo/snapshot/coordinator/session"
)

const (
	restoreGroupUID = types.UID("restore-group-uid")
	capturedGroup   = types.UID("captured-group-uid")
)

// restoreGroup is a PodGroupRestore of a two-member PodGroupSnapshot, leader
// and worker, into two claimed targets on node-a, pinned unless unpinned.
type restoreGroup struct {
	captured *snapshotv1alpha1.PodGroupSnapshot
	restore  *snapshotv1alpha1.PodGroupRestore
	snaps    []*snapshotv1alpha1.PodSnapshot
	contents []*snapshotv1alpha1.PodSnapshotContent
	pods     []*corev1.Pod
}

func newRestoreGroup(t *testing.T) *restoreGroup {
	t.Helper()
	now := metav1.Now()
	rg := &restoreGroup{
		captured: &snapshotv1alpha1.PodGroupSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "pair", Namespace: "inference", UID: capturedGroup}},
		restore: &snapshotv1alpha1.PodGroupRestore{
			ObjectMeta: metav1.ObjectMeta{Name: "pair-again", Namespace: "inference", UID: restoreGroupUID, CreationTimestamp: now},
			Spec: snapshotv1alpha1.PodGroupRestoreSpec{
				SnapshotRef:     snapshotv1alpha1.ObjectIdentity{Name: "pair", UID: capturedGroup},
				DeadlineSeconds: 600,
			},
		},
	}
	for rank, id := range captureMemberIDs {
		contentName := "content-" + id
		snap := &snapshotv1alpha1.PodSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "pair-" + id, Namespace: "inference", UID: types.UID("pair-" + id + "-uid")},
			Status: snapshotv1alpha1.PodSnapshotStatus{BoundPodSnapshotContentName: &contentName,
				Conditions: []metav1.Condition{{Type: snapshotv1alpha1.PodSnapshotConditionReady, Status: metav1.ConditionTrue, Reason: "Captured"}}},
		}
		content := &snapshotv1alpha1.PodSnapshotContent{
			ObjectMeta: metav1.ObjectMeta{Name: contentName, UID: types.UID(contentName + "-uid")},
			Spec: snapshotv1alpha1.PodSnapshotContentSpec{
				PodSnapshotRef: snapshotv1alpha1.PodSnapshotReference{Namespace: "inference", Name: snap.Name, UID: snap.UID},
				Source: snapshotv1alpha1.PodSnapshotContentSource{
					PodRef: snapshotv1alpha1.PodReference{Name: fmt.Sprintf("worker-%d", rank), Containers: []string{"main"}}, NodeName: "source-node"},
				Storage: &snapshotv1alpha1.CheckpointStorageBinding{StoreID: "store"},
			},
			Status: snapshotv1alpha1.PodSnapshotContentStatus{
				Conditions: []metav1.Condition{{Type: snapshotv1alpha1.PodSnapshotConditionReady, Status: metav1.ConditionTrue, Reason: "Captured"}},
				Storage: &snapshotv1alpha1.CheckpointStorageStatus{Artifacts: []snapshotv1alpha1.PublishedContainerArtifact{
					{ContainerName: "main", ArtifactHandle: "handle-" + id}}},
			},
		}
		rg.snaps, rg.contents = append(rg.snaps, snap), append(rg.contents, content)
		rg.captured.Spec.Members = append(rg.captured.Spec.Members, snapshotv1alpha1.PodGroupSnapshotMember{ID: id})
		rg.captured.Status.Members = append(rg.captured.Status.Members, snapshotv1alpha1.PodGroupSnapshotMemberStatus{
			ID: id, Rank: int32(rank), PodSnapshot: &snapshotv1alpha1.ObjectIdentity{Name: snap.Name, UID: snap.UID},
		})

		pod := groupTarget(t, fmt.Sprintf("target-%d", rank), fmt.Sprintf("10.1.0.%d", rank+1))
		rg.pods = append(rg.pods, pod)
		rg.restore.Spec.Targets = append(rg.restore.Spec.Targets, snapshotv1alpha1.PodGroupRestoreTarget{
			ID: id, PodRef: snapshotv1alpha1.TargetPodReference{Name: pod.Name},
		})
		rg.restore.Status.Members = append(rg.restore.Status.Members, snapshotv1alpha1.PodGroupRestoreMemberStatus{
			ID: id, Pod: &snapshotv1alpha1.ObjectIdentity{Name: pod.Name, UID: pod.UID}, NodeName: "node-a",
			Containers:  []snapshotv1alpha1.PodGroupRestoreContainerStatus{{Name: "engine", ContainerID: pod.Status.ContainerStatuses[0].ContainerID}},
			TargetPodIP: pod.Status.PodIP, Phase: snapshotv1alpha1.RestorePhaseAdmitted,
		})
	}
	return rg
}

// groupTarget is a group target claimed by the restore, running on node-a.
func groupTarget(t *testing.T, name, ip string) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "inference", UID: types.UID(name + "-uid")},
		Spec:       corev1.PodSpec{NodeName: testNodeName, Containers: []corev1.Container{{Name: "engine", Image: "workload:1"}}},
	}
	built, err := podcontract.BuildGroupTarget(pod, podcontract.GroupTargetRequest{DestinationContainer: "engine"}, podcontract.Options{})
	require.NoError(t, err)
	claimed, err := podcontract.ClaimGroupTarget(built, podcontract.RestoreGroupClaim{
		UID: restoreGroupUID, Mapping: podcontract.ContainerMapping{Source: "main", Destination: "engine"},
	})
	require.NoError(t, err)
	claimed.Status = corev1.PodStatus{
		Phase: corev1.PodRunning,
		PodIP: ip,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "engine", ContainerID: "containerd://" + name + "-engine",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
		}},
	}
	return claimed
}

func (rg *restoreGroup) unpin() { rg.restore.Status.Members = nil }

// groupRestoreController is a controller with pod groups on, whose targets
// join srv, and whose restores restoreFn runs.
func groupRestoreController(t *testing.T, srv *coordinatortest.Coordinator, rg *restoreGroup,
	restoreFn func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (executor.RestoreResult, error),
) (*NodeController, *kills, *sentinels) {
	t.Helper()
	objects := []runtime.Object{rg.captured}
	for i := range rg.snaps {
		objects = append(objects, rg.snaps[i], rg.contents[i])
	}
	w := makeTestController(t, nil, objects...)
	pods := make([]runtime.Object, len(rg.pods))
	for i := range rg.pods {
		pods[i] = rg.pods[i]
	}
	w.clientset = fake.NewClientset(pods...)
	w.log = testr.New(t)
	w.runtime = &fakeRuntime{resolveContainerPID: 7}
	w.restoreFn = restoreFn
	k := &kills{}
	w.sendSignalFn = k.signal
	s := &sentinels{}
	w.writeControlSentinelFn = s.write
	w.controlSentinelExistsFn = s.exists
	restores := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{uidIndex: func(obj any) ([]string, error) {
		accessor, err := metaAccessor(obj)
		if err != nil {
			return nil, nil
		}
		return []string{string(accessor.GetUID())}, nil
	}})
	require.NoError(t, restores.Add(mustUnstructured(t, rg.restore)))
	w.podGroups = &podGroups{
		configFn: func(ctx context.Context, attempt, member string, sessionNode group.SessionNode) (coordinator.Config, error) {
			node, err := sessionNode(ctx)
			if err != nil {
				return coordinator.Config{}, err
			}
			if node != "node-a" {
				return coordinator.Config{}, fmt.Errorf("the session's node is %q", node)
			}
			if srv == nil {
				return coordinator.Config{}, errors.New("no coordinator in this test")
			}
			return srv.Config(types.UID(attempt), member), nil
		},
		joinFn:   group.Join,
		restores: restores,
	}
	return w, k, s
}

// sentinels records the control files a test's controller writes, and
// answers whether restore-complete exists.
type sentinels struct {
	mu       sync.Mutex
	written  map[string]string
	complete bool
}

func (s *sentinels) write(pid int, name string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.written == nil {
		s.written = map[string]string{}
	}
	s.written[fmt.Sprintf("%d/%s", pid, name)] += string(data)
	return nil
}

func (s *sentinels) exists(int, string) (bool, error) { return s.complete, nil }

func (s *sentinels) get(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.written[key]
}

// memberRestore stands in for the executor's part in a group restore, as
// executor.Restore plays it: admission with the capture's ID, the admitted
// barrier, the commit callback, then the first irreversible step and the
// release barrier. A member that fails ends the attempt.
type memberRestore struct {
	w        func() *NodeController
	refuse   map[string]bool
	panics   map[string]bool
	mu       sync.Mutex
	timeline []string
}

func (m *memberRestore) record(step string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.timeline = append(m.timeline, step)
}

func (m *memberRestore) steps() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.timeline...)
}

func (m *memberRestore) fn(ctx context.Context, _ snapshotruntime.Runtime, _ logr.Logger, req executor.RestoreRequest, _ executor.RestoreMounter) (_ executor.RestoreResult, retErr error) {
	g := req.Group
	if g == nil {
		return executor.RestoreResult{}, errors.New("a target's restore needs its group")
	}
	defer func() {
		if retErr != nil {
			_ = g.Fail(context.WithoutCancel(ctx))
		}
	}()
	if m.refuse[g.ID()] {
		return executor.RestoreResult{}, errors.New("the placeholder is not ready")
	}
	if err := g.Admit(ctx, group.Admission{PodIP: netip.MustParseAddr(req.TargetPodIP), CaptureID: string(capturedGroup)}); err != nil {
		return executor.RestoreResult{}, err
	}
	if err := g.Barrier(ctx, group.Admitted); err != nil {
		return executor.RestoreResult{}, err
	}
	m.record(fmt.Sprintf("%s admitted: %s", g.ID(), m.report(req.PodName)))
	if err := req.Committed(ctx); err != nil {
		return executor.RestoreResult{}, err
	}
	m.record(fmt.Sprintf("%s committed: %s", g.ID(), m.report(req.PodName)))
	if m.panics[g.ID()] {
		panic("restore panicked after the member started")
	}
	if err := g.Barrier(ctx, group.Release); err != nil {
		return executor.RestoreResult{}, fmt.Errorf("barrier: %w", err)
	}
	m.record(g.ID() + " released")
	return executor.RestoreResult{RestoredPID: 42, PlaceholderHostPID: 7}, nil
}

// report reads what the Pod reports, as the API server has it.
func (m *memberRestore) report(name string) string {
	pod, err := m.w().clientset.CoreV1().Pods("inference").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return err.Error()
	}
	started := "not started"
	if restoreStarted(pod) {
		started = "started"
	}
	restored := "no report"
	if c := findRestoredCondition(pod); c != nil {
		restored = c.Reason
	}
	return started + ", " + restored
}

// restoreBoth serves both targets at once, as the restore queue's per-Pod
// goroutines do, and returns what each asked for.
func restoreBoth(t *testing.T, w *NodeController, rg *restoreGroup) []bool {
	t.Helper()
	requeued := make([]bool, len(rg.pods))
	var wg sync.WaitGroup
	for i, pod := range rg.pods {
		wg.Go(func() { requeued[i] = serveTarget(t, w, pod) })
	}
	wg.Wait()
	return requeued
}

func serveTarget(t *testing.T, w *NodeController, pod *corev1.Pod) bool {
	t.Helper()
	current, err := w.clientset.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	claim, _, err := podcontract.RestoreGroupClaimOf(current.Annotations)
	return w.processGroupRestorePod(within(t), current, claim, err)
}

func podCondition(t *testing.T, w *NodeController, name string, conditionType string) *corev1.PodCondition {
	t.Helper()
	pod, err := w.clientset.CoreV1().Pods("inference").Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	for i := range pod.Status.Conditions {
		if string(pod.Status.Conditions[i].Type) == conditionType {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}

func TestGroupRestoreOfTwoMembersThroughTheirSession(t *testing.T) {
	rg := newRestoreGroup(t)
	srv := ensureSession(t, restoreGroupUID, captureMemberIDs)
	var w *NodeController
	executorFake := &memberRestore{w: func() *NodeController { return w }}
	w, k, s := groupRestoreController(t, srv, rg, executorFake.fn)

	assert.Equal(t, []bool{false, false}, restoreBoth(t, w, rg))
	waitForLeaving(t, w)

	assert.ElementsMatch(t, []string{
		"leader admitted: not started, no report", "leader committed: started, RestoreInProgress", "leader released",
		"worker admitted: not started, no report", "worker committed: started, RestoreInProgress", "worker released",
	}, executorFake.steps(), "RestoreStarted is written between the admitted barrier and the member's next step")
	for _, pod := range rg.pods {
		restored := podCondition(t, w, pod.Name, podcontract.RestoredCondition)
		require.NotNil(t, restored)
		assert.Equal(t, corev1.ConditionTrue, restored.Status)
		assert.Equal(t, podcontract.RestoreReasonSucceeded, restored.Reason)
		assert.NotNil(t, podCondition(t, w, pod.Name, podcontract.RestoreStartedCondition), "the record is never removed")
		assert.Nil(t, podCondition(t, w, pod.Name, podcontract.GroupRestoreFailedCondition))
	}
	assert.Equal(t, "pid=42\nattempt=restore-group-uid\npid=42\nattempt=restore-group-uid\n",
		s.get("7/"+podcontract.RestoreCompleteFile), "restore-complete names the attempt that released the member")
	assert.Empty(t, k.list())
	assertFieldManagers(t, w)
}

// assertFieldManagers checks that RestoreStarted went out under its own field
// manager, and every other report under the restore's.
func assertFieldManagers(t *testing.T, w *NodeController) {
	t.Helper()
	for _, action := range w.clientset.(*fake.Clientset).Actions() {
		patch, ok := action.(clientgotesting.PatchActionImpl)
		if !ok || patch.GetSubresource() != "status" {
			continue
		}
		manager := patch.PatchOptions.FieldManager
		if strings.Contains(string(patch.GetPatch()), podcontract.RestoreStartedCondition) {
			assert.Equal(t, targetstatus.StartedFieldManager, manager)
		} else {
			assert.Equal(t, targetstatus.RestoreFieldManager, manager)
		}
	}
}

func TestGroupRestoreWaitsForItsPin(t *testing.T) {
	rg := newRestoreGroup(t)
	rg.unpin()
	restoreFn := &memberRestore{}
	w, _, _ := groupRestoreController(t, nil, rg, restoreFn.fn)

	assert.False(t, serveTarget(t, w, rg.pods[0]), "the PodGroupRestore's update brings the target back")
	restored := podCondition(t, w, rg.pods[0].Name, podcontract.RestoredCondition)
	require.NotNil(t, restored)
	assert.Equal(t, corev1.ConditionFalse, restored.Status)
	assert.Equal(t, reasonGroupPending, restored.Reason)
	assert.False(t, podcontract.ClassifyRestoreOutcome([]corev1.PodCondition{*restored}).Terminal(),
		"the agent's own wait is not an outcome")
	assert.Empty(t, restoreFn.steps())

	actions := len(w.clientset.(*fake.Clientset).Actions())
	assert.False(t, serveTarget(t, w, rg.pods[0]))
	assert.Len(t, w.clientset.(*fake.Clientset).Actions(), actions+1, "an unchanged wait is not written again")
}

func TestGroupRestoreReasons(t *testing.T) {
	t.Run("a member that fails because the attempt ended reports the attempt's reason", func(t *testing.T) {
		rg := newRestoreGroup(t)
		srv := ensureSession(t, restoreGroupUID, captureMemberIDs)
		var w *NodeController
		executorFake := &memberRestore{w: func() *NodeController { return w }, refuse: map[string]bool{"worker": true}}
		w, k, _ := groupRestoreController(t, srv, rg, executorFake.fn)

		restoreBoth(t, w, rg)

		leaderRestored := podCondition(t, w, "target-0", podcontract.RestoredCondition)
		assert.Equal(t, podcontract.RestoreReasonFailed, leaderRestored.Reason)
		groupFailed := podCondition(t, w, "target-0", podcontract.GroupRestoreFailedCondition)
		require.NotNil(t, groupFailed, "the attempt's reason goes on GroupRestoreFailed")
		assert.Equal(t, "Refused", groupFailed.Reason)
		assert.Equal(t, leaderRestored.Message, groupFailed.Message, "written in the same report as Restored")

		workerRestored := podCondition(t, w, "target-1", podcontract.RestoredCondition)
		assert.Equal(t, podcontract.RestoreReasonFailed, workerRestored.Reason)
		assert.Nil(t, podCondition(t, w, "target-1", podcontract.GroupRestoreFailedCondition),
			"a member that fails on its own leaves GroupRestoreFailed unset")
		for _, pod := range rg.pods {
			assert.Nil(t, podCondition(t, w, pod.Name, podcontract.RestoreStartedCondition))
		}
		assert.Empty(t, k.list(), "members that never started are left as found")
	})

	for name, tc := range map[string]struct {
		modify func(*restoreGroup)
		want   string
	}{
		"a Pod IP that changed since the pin": {
			modify: func(rg *restoreGroup) { rg.pods[0].Status.PodIP = "10.1.0.99" },
			want:   snapshotv1alpha1.PodGroupReasonPodIPChanged,
		},
		"a container that changed since the pin": {
			modify: func(rg *restoreGroup) { rg.pods[0].Status.ContainerStatuses[0].ContainerID = "containerd://another" },
			want:   snapshotv1alpha1.PodGroupReasonPodIPChanged,
		},
	} {
		t.Run(name, func(t *testing.T) {
			rg := newRestoreGroup(t)
			tc.modify(rg)
			restoreFn := &memberRestore{}
			w, _, _ := groupRestoreController(t, nil, rg, restoreFn.fn)
			joins := countJoins(w)

			assert.False(t, serveTarget(t, w, rg.pods[0]))

			assert.Empty(t, restoreFn.steps(), "a member that fails a group check never joins its restore")
			assert.Equal(t, podcontract.RestoreReasonFailed, podCondition(t, w, "target-0", podcontract.RestoredCondition).Reason)
			assert.Equal(t, tc.want, podCondition(t, w, "target-0", podcontract.GroupRestoreFailedCondition).Reason)
			assert.Zero(t, joins.Load(), "its failure on the Pod is all its peers learn of it; it does not join to refuse")
		})
	}
}

// countJoins counts the attempts that w's targets set out to join.
func countJoins(w *NodeController) *atomic.Int32 {
	joins := &atomic.Int32{}
	config := w.podGroups.configFn
	w.podGroups.configFn = func(ctx context.Context, attempt, member string, sessionNode group.SessionNode) (coordinator.Config, error) {
		joins.Add(1)
		return config(ctx, attempt, member, sessionNode)
	}
	return joins
}

func TestGroupRestoreReportsEveryPreflightFailure(t *testing.T) {
	for name, tc := range map[string]struct {
		// target returns the Pod to serve, which it adds to rg's Pods.
		target func(*testing.T, *restoreGroup) *corev1.Pod
		want   string
	}{
		"a claimed Pod that its PodGroupRestore has no target for": {
			target: func(t *testing.T, rg *restoreGroup) *corev1.Pod {
				rg.pods = append(rg.pods, groupTarget(t, "stranger", "10.1.0.9"))
				return rg.pods[len(rg.pods)-1]
			},
			want: "has no target for it",
		},
		"a Pod that is not Pending or Running": {
			target: func(_ *testing.T, rg *restoreGroup) *corev1.Pod {
				rg.pods[0].Status.Phase = corev1.PodSucceeded
				return rg.pods[0]
			},
			want: "expected Pending or Running",
		},
	} {
		t.Run(name, func(t *testing.T) {
			rg := newRestoreGroup(t)
			pod := tc.target(t, rg)
			restoreFn := &memberRestore{}
			w, _, _ := groupRestoreController(t, nil, rg, restoreFn.fn)
			joins := countJoins(w)

			assert.False(t, serveTarget(t, w, pod))

			restored := podCondition(t, w, pod.Name, podcontract.RestoredCondition)
			require.NotNil(t, restored, "the failure is reported on the Pod, as every preflight failure is")
			assert.Equal(t, corev1.ConditionFalse, restored.Status)
			assert.Equal(t, podcontract.RestoreReasonFailed, restored.Reason)
			assert.Contains(t, restored.Message, tc.want)
			assert.Empty(t, restoreFn.steps())
			assert.Zero(t, joins.Load())
		})
	}
}

// failReads makes w's reads of one kind fail with the error that fail
// returns for it, while fail returns one.
func failReads(w *NodeController, fail func(client.Object) error) {
	w.client = interceptor.NewClient(w.client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := fail(obj); err != nil {
				return err
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
}

func TestGroupRestoreWaitsOutATransientRead(t *testing.T) {
	rg := newRestoreGroup(t)
	srv := ensureSession(t, restoreGroupUID, captureMemberIDs)
	var w *NodeController
	executorFake := &memberRestore{w: func() *NodeController { return w }}
	w, k, _ := groupRestoreController(t, srv, rg, executorFake.fn)
	var failing atomic.Value
	failing.Store("")
	failReads(w, func(obj client.Object) error {
		switch obj.(type) {
		case *snapshotv1alpha1.PodGroupSnapshot:
			if failing.Load() == "PodGroupSnapshot" {
				return apierrors.NewTooManyRequests("the API server is throttling", 1)
			}
		case *snapshotv1alpha1.PodSnapshot:
			if failing.Load() == "PodSnapshot" {
				return apierrors.NewServerTimeout(schema.GroupResource{Group: "nvidia.com", Resource: "podsnapshots"}, "get", 1)
			}
		}
		return nil
	})

	for kind, want := range map[string]string{
		"PodGroupSnapshot": "Unable to read PodGroupSnapshot inference/pair; retrying",
		"PodSnapshot":      "Unable to read PodSnapshot inference/pair-leader of member leader; retrying",
	} {
		failing.Store(kind)
		assert.True(t, serveTarget(t, w, rg.pods[0]), "a failed read of the %s is tried again", kind)
		restored := podCondition(t, w, "target-0", podcontract.RestoredCondition)
		require.NotNil(t, restored)
		assert.Equal(t, reasonGroupPending, restored.Reason)
		assert.Equal(t, want, restored.Message)
		assert.Nil(t, podCondition(t, w, "target-0", podcontract.GroupRestoreFailedCondition))
	}
	waitForLeaving(t, w)
	assert.Empty(t, executorFake.steps())

	// Nothing was refused: once the reads succeed, both members restore.
	failing.Store("")
	assert.Equal(t, []bool{false, false}, restoreBoth(t, w, rg))
	waitForLeaving(t, w)
	for _, pod := range rg.pods {
		assert.Equal(t, podcontract.RestoreReasonSucceeded, podCondition(t, w, pod.Name, podcontract.RestoredCondition).Reason)
	}
	assert.Empty(t, k.list())
}

func TestGroupRestoreFailsWhenItsSnapshotIsGone(t *testing.T) {
	for kind, gone := range map[string]func(client.Object) bool{
		"PodGroupSnapshot": func(obj client.Object) bool { _, ok := obj.(*snapshotv1alpha1.PodGroupSnapshot); return ok },
		"PodSnapshot":      func(obj client.Object) bool { _, ok := obj.(*snapshotv1alpha1.PodSnapshot); return ok },
	} {
		t.Run(kind, func(t *testing.T) {
			rg := newRestoreGroup(t)
			restoreFn := &memberRestore{}
			w, _, _ := groupRestoreController(t, nil, rg, restoreFn.fn)
			joins := countJoins(w)
			failReads(w, func(obj client.Object) error {
				if gone(obj) {
					return apierrors.NewNotFound(schema.GroupResource{Group: "nvidia.com"}, obj.GetName())
				}
				return nil
			})

			assert.False(t, serveTarget(t, w, rg.pods[0]))

			restored := podCondition(t, w, "target-0", podcontract.RestoredCondition)
			assert.Equal(t, podcontract.RestoreReasonFailed, restored.Reason)
			assert.Contains(t, restored.Message, "is gone")
			assert.Empty(t, restoreFn.steps())
			assert.Zero(t, joins.Load(), "a snapshot that is gone fails the member on its Pod only")
		})
	}
}

func TestGroupRestoreThatCannotRecordItsStartFailsTheAttempt(t *testing.T) {
	rg := newRestoreGroup(t)
	srv := ensureSession(t, restoreGroupUID, captureMemberIDs)
	var w *NodeController
	executorFake := &memberRestore{w: func() *NodeController { return w }}
	w, k, s := groupRestoreController(t, srv, rg, executorFake.fn)
	w.clientset.(*fake.Clientset).PrependReactor("patch", "pods", func(action clientgotesting.Action) (bool, runtime.Object, error) {
		patch := action.(clientgotesting.PatchAction)
		if patch.GetName() == "target-1" && strings.Contains(string(patch.GetPatch()), podcontract.RestoreStartedCondition) {
			return true, nil, errors.New("the API server is unavailable")
		}
		return false, nil, nil
	})

	restoreBoth(t, w, rg)

	assert.Nil(t, podCondition(t, w, "target-1", podcontract.RestoreStartedCondition))
	assert.Equal(t, podcontract.RestoreReasonFailed, podCondition(t, w, "target-1", podcontract.RestoredCondition).Reason)
	assert.Nil(t, podCondition(t, w, "target-1", podcontract.GroupRestoreFailedCondition), "its own failure")

	assert.Equal(t, podcontract.RestoreReasonFailed, podCondition(t, w, "target-0", podcontract.RestoredCondition).Reason)
	assert.Equal(t, session.ReasonMemberFailed, podCondition(t, w, "target-0", podcontract.GroupRestoreFailedCondition).Reason,
		"the leader reports the attempt's reason")
	assert.NotContains(t, executorFake.steps(), "leader released")
	// The leader may learn that the attempt failed before its admitted barrier
	// returns, and then never start. Both outcomes are legitimate; only a
	// member that started is failed, which ends its processes.
	if podCondition(t, w, "target-0", podcontract.RestoreStartedCondition) != nil {
		assert.Equal(t, []int{7}, k.list(), "only the member that started is failed, which ends its processes")
		assert.Equal(t, string(restoreFailedSentinelContents), s.get("7/"+podcontract.RestoreFailedFile))
	} else {
		assert.Empty(t, k.list(), "no member started, so no placeholder is touched")
		assert.Empty(t, s.get("7/"+podcontract.RestoreFailedFile), "a leader that never started is left as found")
	}
}

func TestGroupRestoreThatPanicsAfterItStartedFailsTheMember(t *testing.T) {
	rg := newRestoreGroup(t)
	srv := ensureSession(t, restoreGroupUID, captureMemberIDs)
	var w *NodeController
	executorFake := &memberRestore{w: func() *NodeController { return w }, panics: map[string]bool{"leader": true}}
	w, k, s := groupRestoreController(t, srv, rg, executorFake.fn)

	restoreBoth(t, w, rg)

	restored := podCondition(t, w, "target-0", podcontract.RestoredCondition)
	assert.Equal(t, podcontract.RestoreReasonFailed, restored.Reason)
	assert.Contains(t, restored.Message, "panic")
	assert.Equal(t, session.ReasonMemberFailed, podCondition(t, w, "target-1", podcontract.GroupRestoreFailedCondition).Reason,
		"the attempt fails for the peer")
	// The fake runtime gives both placeholders PID 7. The leader started, so
	// its placeholder learns that no restore is coming, and is killed. The
	// peer may learn that the attempt failed before its admitted barrier
	// returns, and then never start. Both outcomes are legitimate; only a
	// member that started is failed, which ends its processes.
	if podCondition(t, w, "target-1", podcontract.RestoreStartedCondition) != nil {
		assert.Equal(t, strings.Repeat(string(restoreFailedSentinelContents), 2), s.get("7/"+podcontract.RestoreFailedFile))
		assert.Equal(t, []int{7, 7}, k.list())
	} else {
		assert.Equal(t, string(restoreFailedSentinelContents), s.get("7/"+podcontract.RestoreFailedFile),
			"a peer that never started is left as found")
		assert.Equal(t, []int{7}, k.list())
	}
}

func TestGroupRestoreIsBoundByTheGroupsDeadlineOnly(t *testing.T) {
	rg := newRestoreGroup(t)
	srv := ensureSession(t, restoreGroupUID, captureMemberIDs)
	var deadline time.Time
	w, _, _ := groupRestoreController(t, srv, rg,
		func(ctx context.Context, _ snapshotruntime.Runtime, _ logr.Logger, _ executor.RestoreRequest, _ executor.RestoreMounter) (executor.RestoreResult, error) {
			deadline, _ = ctx.Deadline()
			return executor.RestoreResult{}, errors.New("stop once the deadline is read")
		})
	w.config.Restore.TimeoutSeconds = 60
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	claim, _, err := podcontract.RestoreGroupClaimOf(rg.pods[0].Annotations)
	require.NoError(t, err)

	w.processGroupRestorePod(ctx, rg.pods[0], claim, nil)
	assert.WithinDuration(t, rg.restore.CreationTimestamp.Add(600*time.Second), deadline, time.Second,
		"restore.timeoutSeconds does not bound a member, so it fails as the group does")
}

func TestGroupRestoreAfterARestart(t *testing.T) {
	started := func(pod *corev1.Pod) {
		pod.Status.Conditions = append(pod.Status.Conditions,
			corev1.PodCondition{Type: podcontract.RestoreStartedCondition, Status: corev1.ConditionTrue, Reason: "RestoreStarted"},
			corev1.PodCondition{Type: podcontract.RestoredCondition, Status: corev1.ConditionFalse, Reason: podcontract.RestoreReasonInProgress})
	}

	t.Run("a member that started but was not released fails", func(t *testing.T) {
		rg := newRestoreGroup(t)
		started(rg.pods[0])
		restoreFn := &memberRestore{}
		w, k, s := groupRestoreController(t, nil, rg, restoreFn.fn)

		assert.False(t, serveTarget(t, w, rg.pods[0]))
		assert.Empty(t, restoreFn.steps(), "CRIU never runs again into a started Pod")
		restored := podCondition(t, w, "target-0", podcontract.RestoredCondition)
		assert.Equal(t, podcontract.RestoreReasonFailed, restored.Reason)
		assert.Contains(t, restored.Message, "never restored again")
		assert.Equal(t, []int{7}, k.list())
		assert.Equal(t, string(restoreFailedSentinelContents), s.get("7/"+podcontract.RestoreFailedFile))
		assert.NotNil(t, podCondition(t, w, "target-0", podcontract.RestoreStartedCondition))
	})

	t.Run("a released member waits out a failed read of its snapshot", func(t *testing.T) {
		rg := newRestoreGroup(t)
		started(rg.pods[0])
		restoreFn := &memberRestore{}
		w, k, s := groupRestoreController(t, nil, rg, restoreFn.fn)
		s.complete = true
		failReads(w, func(obj client.Object) error {
			if _, ok := obj.(*snapshotv1alpha1.PodGroupSnapshot); ok {
				return apierrors.NewTooManyRequests("the API server is throttling", 1)
			}
			return nil
		})

		assert.True(t, serveTarget(t, w, rg.pods[0]), "its post-restore action needs the snapshot, so it tries again")
		assert.Equal(t, podcontract.RestoreReasonInProgress, podCondition(t, w, "target-0", podcontract.RestoredCondition).Reason)
		assert.Empty(t, k.list())
	})

	t.Run("a member that was released keeps running", func(t *testing.T) {
		rg := newRestoreGroup(t)
		started(rg.pods[0])
		restoreFn := &memberRestore{}
		w, k, s := groupRestoreController(t, nil, rg, restoreFn.fn)
		s.complete = true

		assert.False(t, serveTarget(t, w, rg.pods[0]))
		assert.Empty(t, restoreFn.steps())
		assert.Equal(t, podcontract.RestoreReasonSucceeded, podCondition(t, w, "target-0", podcontract.RestoredCondition).Reason)
		assert.Empty(t, k.list())
	})

	t.Run("an outcome this agent holds is reported again", func(t *testing.T) {
		rg := newRestoreGroup(t)
		restoreFn := &memberRestore{}
		w, _, _ := groupRestoreController(t, nil, rg, restoreFn.fn)
		w.podGroups.restoreOutcomes.Store(rg.pods[0].UID, failedOutcome(nil, rg.pods[0], session.ReasonCoordinatorLost,
			errors.New("the coordinator was lost")))

		assert.False(t, serveTarget(t, w, rg.pods[0]))
		assert.Empty(t, restoreFn.steps())
		assert.Equal(t, session.ReasonCoordinatorLost, podCondition(t, w, "target-0", podcontract.GroupRestoreFailedCondition).Reason)
	})
}

func TestRestoreMemberOf(t *testing.T) {
	rg := newRestoreGroup(t)
	w, _, _ := groupRestoreController(t, nil, rg, nil)
	claim, bound, err := podcontract.RestoreGroupClaimOf(rg.pods[1].Annotations)
	require.NoError(t, err)
	require.True(t, bound)

	m, err := w.restoreMemberOf(rg.pods[1], claim)
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, "worker", m.id)
	assert.Equal(t, restoreGroupUID, m.attempt())
	require.NotNil(t, m.pin)
	assert.Equal(t, rg.pods[1].UID, fence(m, rg.pods[1]))
	assert.WithinDuration(t, rg.restore.CreationTimestamp.Add(600*time.Second), m.deadline, time.Second)
	node, err := w.restoreSessionNode(m)(within(t))
	require.NoError(t, err)
	assert.Equal(t, "node-a", node)

	stranger := groupTarget(t, "stranger", "10.1.0.9")
	_, err = w.restoreMemberOf(stranger, claim)
	require.ErrorContains(t, err, "has no target for it")

	other := claim
	other.UID = "unknown-uid"
	m, err = w.restoreMemberOf(rg.pods[0], other)
	require.NoError(t, err)
	assert.Nil(t, m, "a PodGroupRestore the agent has not seen is waited for")

	unpinned := newRestoreGroup(t)
	unpinned.unpin()
	w, _, _ = groupRestoreController(t, nil, unpinned, nil)
	m, err = w.restoreMemberOf(unpinned.pods[0], claim)
	require.NoError(t, err)
	assert.Nil(t, m.pin)
	assert.Equal(t, unpinned.pods[0].UID, fence(m, unpinned.pods[0]), "before the pin, writes are fenced on the Pod read")
}

func TestCheckGroupPin(t *testing.T) {
	for name, tc := range map[string]struct {
		// modify changes the Pod, as the API server and the agent's cache both
		// hold it, and its pin.
		modify func(*corev1.Pod, *snapshotv1alpha1.PodGroupRestoreMemberStatus)
		// stale changes the Pod in the agent's cache only.
		stale      func(*corev1.Pod)
		wantReason string
		pending    bool
		// reads is how many times the check reads the Pod from the API server.
		reads int
	}{
		"the pinned Pod and container": {},
		"another Pod": {
			modify:     func(_ *corev1.Pod, pin *snapshotv1alpha1.PodGroupRestoreMemberStatus) { pin.Pod.UID = "replaced" },
			wantReason: snapshotv1alpha1.PodGroupReasonPodIPChanged,
			reads:      1,
		},
		"another IP": {
			modify:     func(pod *corev1.Pod, _ *snapshotv1alpha1.PodGroupRestoreMemberStatus) { pod.Status.PodIP = "10.9.9.9" },
			wantReason: snapshotv1alpha1.PodGroupReasonPodIPChanged,
			reads:      1,
		},
		"another container": {
			modify: func(pod *corev1.Pod, _ *snapshotv1alpha1.PodGroupRestoreMemberStatus) {
				pod.Status.ContainerStatuses[0].ContainerID = "containerd://restarted"
			},
			wantReason: snapshotv1alpha1.PodGroupReasonPodIPChanged,
			reads:      1,
		},
		"a pin that names no container": {
			modify:     func(_ *corev1.Pod, pin *snapshotv1alpha1.PodGroupRestoreMemberStatus) { pin.Containers = nil },
			wantReason: snapshotv1alpha1.PodGroupReasonPodIPChanged,
		},
		"a destination that is not running": {
			modify: func(pod *corev1.Pod, _ *snapshotv1alpha1.PodGroupRestoreMemberStatus) {
				pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}}
			},
			pending: true,
		},
		"an IP the cache does not hold yet": {
			stale:   func(pod *corev1.Pod) { pod.Status.PodIP = "" },
			pending: true,
		},
		"an IP the cache holds wrong, which the API server clears": {
			stale:   func(pod *corev1.Pod) { pod.Status.PodIP = "10.9.9.9" },
			pending: true,
			reads:   1,
		},
		"a Pod UID the cache holds wrong, which the API server clears": {
			stale:   func(pod *corev1.Pod) { pod.UID = "an-older-pod" },
			pending: true,
			reads:   1,
		},
		"a container ID the cache holds from before a restart, which the API server clears": {
			stale:   func(pod *corev1.Pod) { pod.Status.ContainerStatuses[0].ContainerID = "containerd://before-the-restart" },
			pending: true,
			reads:   1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			rg := newRestoreGroup(t)
			if tc.modify != nil {
				tc.modify(rg.pods[0], &rg.restore.Status.Members[0])
			}
			w, _, _ := groupRestoreController(t, nil, rg, nil)
			claim, _, _ := podcontract.RestoreGroupClaimOf(rg.pods[0].Annotations)
			m, err := w.restoreMemberOf(rg.pods[0], claim)
			require.NoError(t, err)
			pod := rg.pods[0].DeepCopy()
			if tc.stale != nil {
				tc.stale(pod)
			}

			containerID, err := w.checkGroupPin(within(t), pod, m)
			assert.Equal(t, tc.reads, podReads(w), "a mismatch in the cache is confirmed with one read")
			switch {
			case tc.pending:
				assert.Equal(t, reasonGroupPending, pendingRestoreReason(t, err))
			case tc.wantReason != "":
				var check *groupCheckError
				require.ErrorAs(t, err, &check)
				assert.Equal(t, tc.wantReason, check.reason)
			default:
				require.NoError(t, err)
				assert.Equal(t, "target-0-engine", containerID)
			}
		})
	}
}

// podReads counts w's reads of a Pod from the API server.
func podReads(w *NodeController) int {
	reads := 0
	for _, action := range w.clientset.(*fake.Clientset).Actions() {
		if action.GetVerb() == "get" && action.GetResource().Resource == "pods" {
			reads++
		}
	}
	return reads
}

func TestCheckGroupPinConfirmsAMismatch(t *testing.T) {
	for name, tc := range map[string]struct {
		// apiServer changes the Pod, or the answer to the agent's read of it,
		// on the API server only.
		apiServer  func(*fake.Clientset, *corev1.Pod)
		wantReason string
		wantWait   string
	}{
		"a read that fails": {
			apiServer: func(clientset *fake.Clientset, _ *corev1.Pod) {
				clientset.PrependReactor("get", "pods", func(clientgotesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("the API server is unavailable")
				})
			},
			wantWait: "Unable to read Pod inference/target-0; retrying",
		},
		"a Pod that is gone": {
			apiServer: func(clientset *fake.Clientset, pod *corev1.Pod) {
				require.NoError(t, clientset.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{}))
			},
			wantReason: snapshotv1alpha1.PodGroupReasonPodIPChanged,
		},
		"a Pod that was replaced": {
			apiServer: func(clientset *fake.Clientset, pod *corev1.Pod) {
				replaced := pod.DeepCopy()
				replaced.UID = "replaced"
				require.NoError(t, clientset.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), replaced, pod.Namespace))
			},
			wantReason: snapshotv1alpha1.PodGroupReasonPodIPChanged,
		},
		"a Pod whose IP is not the pinned one": {
			apiServer: func(clientset *fake.Clientset, pod *corev1.Pod) {
				changed := pod.DeepCopy()
				changed.Status.PodIP = "10.9.9.9"
				require.NoError(t, clientset.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), changed, pod.Namespace))
			},
			wantReason: snapshotv1alpha1.PodGroupReasonPodIPChanged,
		},
		"a Pod whose destination is not the pinned container": {
			apiServer: func(clientset *fake.Clientset, pod *corev1.Pod) {
				changed := pod.DeepCopy()
				changed.Status.ContainerStatuses[0].ContainerID = "containerd://restarted"
				require.NoError(t, clientset.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), changed, pod.Namespace))
			},
			wantReason: snapshotv1alpha1.PodGroupReasonPodIPChanged,
		},
		"a Pod that waits for its destination to run again": {
			apiServer: func(clientset *fake.Clientset, pod *corev1.Pod) {
				changed := pod.DeepCopy()
				changed.Status.ContainerStatuses[0].ContainerID = ""
				require.NoError(t, clientset.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), changed, pod.Namespace))
			},
			wantWait: "Waiting for container engine to run",
		},
		"a Pod that matches the pin": {
			apiServer: func(*fake.Clientset, *corev1.Pod) {},
			wantWait:  "Waiting for the node agent to see the pinned Pod",
		},
	} {
		t.Run(name, func(t *testing.T) {
			rg := newRestoreGroup(t)
			w, _, _ := groupRestoreController(t, nil, rg, nil)
			claim, _, _ := podcontract.RestoreGroupClaimOf(rg.pods[0].Annotations)
			m, err := w.restoreMemberOf(rg.pods[0], claim)
			require.NoError(t, err)
			tc.apiServer(w.clientset.(*fake.Clientset), rg.pods[0])
			cached := rg.pods[0].DeepCopy()
			cached.Status.PodIP = "10.9.9.9"

			_, err = w.checkGroupPin(within(t), cached, m)
			assert.Equal(t, 1, podReads(w), "one read decides")
			if tc.wantWait != "" {
				var pending *restorePendingError
				require.ErrorAs(t, err, &pending)
				assert.Equal(t, tc.wantWait, pending.message)
				return
			}
			var check *groupCheckError
			require.ErrorAs(t, err, &check)
			assert.Equal(t, tc.wantReason, check.reason)
		})
	}
}

func TestRunChecksThatPodGroupRestoresAreReadable(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"a permission the agent lacks": {
			err:  apierrors.NewForbidden(podGroupRestoreGVR.GroupResource(), "", errors.New("RBAC denied the list")),
			want: "pod groups need permission to list and watch podgrouprestores.nvidia.com in every namespace",
		},
		"credentials the API server refuses": {
			err:  apierrors.NewUnauthorized("the token expired"),
			want: "the API server refused the agent's credentials when it listed podgrouprestores.nvidia.com",
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := makeTestController(t, nil)
			w.podGroups = &podGroups{}
			dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
				map[schema.GroupVersionResource]string{podGroupRestoreGVR: "PodGroupRestoreList"})
			dynClient.PrependReactor("list", "podgrouprestores", func(clientgotesting.Action) (bool, runtime.Object, error) {
				return true, nil, tc.err
			})
			w.dynClient = dynClient

			err := w.Run(within(t))
			require.ErrorContains(t, err, tc.want, "the agent exits rather than wait for a cache that never syncs")
		})
	}
}

func TestRunWithoutGroupRestoreCRDServesStandaloneCapture(t *testing.T) {
	g := newCaptureGroup()
	g.contents[0].Spec.PodGroup = nil
	w := makeTestController(t, nil)
	w.client = crfake.NewClientBuilder().WithScheme(contentScheme(t)).
		WithObjects(g.snaps[0], g.contents[0], g.pods[0]).
		WithStatusSubresource(&snapshotv1alpha1.PodSnapshotContent{}).Build()
	w.config.NodeName = "node-a"
	w.podGroups = &podGroups{}
	w.runtime = &fakeRuntime{resolveContainerPID: 7}
	fc := &fakeCheckpointer{}
	w.checkpointFn = fc.fn
	content := mustUnstructured(t, getContent(t, w, g.contents[0].Name))
	content.SetGroupVersionKind(snapshotv1alpha1.GroupVersion.WithKind("PodSnapshotContent"))
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			podSnapshotContentGVR: "PodSnapshotContentList", podGroupRestoreGVR: "PodGroupRestoreList",
		}, content)
	dynClient.PrependReactor("list", "podgrouprestores", func(clientgotesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(podGroupRestoreGVR.GroupResource(), "")
	})
	w.dynClient = dynClient
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			assert.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("the controller did not stop")
		}
	})

	require.Eventually(t, func() bool {
		return isContentReady(getContent(t, w, content.GetName()))
	}, 5*time.Second, 10*time.Millisecond, "a missing group CRD does not hold standalone work")
	assert.True(t, fc.wasCalled())
}

func TestCheckPodGroupRestoresReadable(t *testing.T) {
	for name, tc := range map[string]struct {
		err      error
		wantErr  bool
		wantSync bool
	}{
		"readable":                           {wantSync: true},
		"a CRD that is not installed":        {err: apierrors.NewNotFound(podGroupRestoreGVR.GroupResource(), "")},
		"a permission the agent lacks":       {err: apierrors.NewForbidden(podGroupRestoreGVR.GroupResource(), "", errors.New("denied")), wantErr: true},
		"credentials the API server refuses": {err: apierrors.NewUnauthorized("expired"), wantErr: true},
		"an API server briefly unavailable":  {err: apierrors.NewServiceUnavailable("starting"), wantSync: true},
	} {
		t.Run(name, func(t *testing.T) {
			w := makeTestController(t, nil)
			dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
				map[schema.GroupVersionResource]string{podGroupRestoreGVR: "PodGroupRestoreList"})
			if tc.err != nil {
				dynClient.PrependReactor("list", "podgrouprestores", func(clientgotesting.Action) (bool, runtime.Object, error) {
					return true, nil, tc.err
				})
			}
			w.dynClient = dynClient

			waitForSync, err := w.checkPodGroupRestoresReadable(within(t))
			assert.Equal(t, tc.wantSync, waitForSync)
			if tc.wantErr {
				require.ErrorIs(t, err, tc.err)
			} else {
				require.NoError(t, err, "the informer retries while standalone work stays available")
			}
		})
	}
}

func TestEnqueueGroupTargets(t *testing.T) {
	rg := newRestoreGroup(t)
	w, _, _ := groupRestoreController(t, nil, rg, nil)
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	other := groupTarget(t, "target-1", "10.1.0.2")
	other.Spec.NodeName = "node-b"
	require.NoError(t, indexer.Add(rg.pods[0]))
	require.NoError(t, indexer.Add(other))
	w.restorePodLister = corev1listers.NewPodLister(indexer)

	w.enqueueGroupTargets(mustUnstructured(t, rg.restore))

	require.Equal(t, 1, w.restoreQueue.Len(), "only the targets on this node are queued")
	key, _ := w.restoreQueue.Get()
	assert.Equal(t, client.ObjectKeyFromObject(rg.pods[0]), key)
}

// markStarted makes pod a target whose member had started when the agent
// restarted: RestoreStarted, and Restored still in progress.
func markStarted(pod *corev1.Pod) {
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: podcontract.RestoreStartedCondition, Status: corev1.ConditionTrue, Reason: "RestoreStarted"},
		{Type: podcontract.RestoredCondition, Status: corev1.ConditionFalse, Reason: podcontract.RestoreReasonInProgress},
	}
}

// withPostRestore gives the leader's checkpoint an HTTP post-restore action on
// port.
func withPostRestore(rg *restoreGroup, port intstr.IntOrString) {
	rg.contents[0].Spec.Containers = []snapshotv1alpha1.ContainerSnapshotConfig{{Name: "main",
		Lifecycle: &snapshotv1alpha1.SnapshotLifecycle{PostRestore: &snapshotv1alpha1.PostRestoreAction{
			PostRestoreHandler: snapshotv1alpha1.PostRestoreHandler{HTTPPost: &snapshotv1alpha1.HTTPPostAction{
				Path: "/resume", Port: port,
			}},
		}},
	}}
}

// notifications counts the post-restore requests a test's controller sends,
// and answers each with 200.
func notifications(w *NodeController) *atomic.Int32 {
	calls := &atomic.Int32{}
	w.hookHTTP = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	return calls
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGroupRestoreRecoveryTrustsOnlyThePinnedContainer(t *testing.T) {
	rg := newRestoreGroup(t)
	markStarted(rg.pods[0])
	// The kubelet restarted the container after the release. The control
	// volume is an emptyDir, so the old restore-complete is still there.
	rg.pods[0].Status.ContainerStatuses[0].ContainerID = "containerd://replacement-container"
	restoreFn := &memberRestore{}
	w, k, s := groupRestoreController(t, nil, rg, restoreFn.fn)
	s.complete = true

	assert.False(t, serveTarget(t, w, rg.pods[0]))

	restored := podCondition(t, w, "target-0", podcontract.RestoredCondition)
	require.NotNil(t, restored)
	assert.Equal(t, corev1.ConditionFalse, restored.Status, "an old restore-complete in a new container proves nothing")
	assert.Equal(t, podcontract.RestoreReasonFailed, restored.Reason)
	assert.Nil(t, podCondition(t, w, "target-0", podcontract.GroupRestoreFailedCondition), "a reason of the member's own")
	events := eventsForReason(w.clientset.(*fake.Clientset), reasonRestoreInterrupted)
	require.Len(t, events, 1)
	assert.Contains(t, events[0].Message, "was replaced")
	assert.Empty(t, restoreFn.steps(), "CRIU never runs again into a started Pod")
	assert.Empty(t, k.list(), "nothing restored runs in the replacement")
}

func TestGroupRestoreRecoveryNeverSkipsARequiredNotification(t *testing.T) {
	for name, tc := range map[string]struct {
		port  intstr.IntOrString
		reads func(client.Object) error
	}{
		"its PodGroupSnapshot is gone": {
			port: intstr.FromInt32(8000),
			reads: func(obj client.Object) error {
				if _, ok := obj.(*snapshotv1alpha1.PodGroupSnapshot); ok {
					return apierrors.NewNotFound(schema.GroupResource{Group: "nvidia.com", Resource: "podgroupsnapshots"}, "pair")
				}
				return nil
			},
		},
		"its PodSnapshot is gone": {
			port: intstr.FromInt32(8000),
			reads: func(obj client.Object) error {
				if _, ok := obj.(*snapshotv1alpha1.PodSnapshot); ok {
					return apierrors.NewNotFound(schema.GroupResource{Group: "nvidia.com", Resource: "podsnapshots"}, obj.GetName())
				}
				return nil
			},
		},
		"its action cannot be planned": {port: intstr.FromString("no-such-port")},
	} {
		t.Run(name, func(t *testing.T) {
			rg := newRestoreGroup(t)
			markStarted(rg.pods[0])
			withPostRestore(rg, tc.port)
			w, k, s := groupRestoreController(t, nil, rg, (&memberRestore{}).fn)
			s.complete = true
			calls := notifications(w)
			if tc.reads != nil {
				failReads(w, tc.reads)
			}

			assert.False(t, serveTarget(t, w, rg.pods[0]))

			restored := podCondition(t, w, "target-0", podcontract.RestoredCondition)
			require.NotNil(t, restored)
			assert.Equal(t, corev1.ConditionFalse, restored.Status, "a notification that never ran is not a success")
			assert.Equal(t, podcontract.RestoreReasonFailed, restored.Reason)
			assert.Contains(t, restored.Message, "post-restore action")
			assert.Nil(t, podCondition(t, w, "target-0", podcontract.GroupRestoreFailedCondition))
			assert.Zero(t, calls.Load())
			// As a post-restore action that failed: the workload ends.
			assert.Equal(t, string(restoreFailedSentinelContents), s.get("7/"+podcontract.RestoreFailedFile))
			assert.Equal(t, []int{7}, k.list())
		})
	}

	t.Run("an action that can be read runs", func(t *testing.T) {
		rg := newRestoreGroup(t)
		markStarted(rg.pods[0])
		withPostRestore(rg, intstr.FromInt32(8000))
		w, k, s := groupRestoreController(t, nil, rg, (&memberRestore{}).fn)
		s.complete = true
		calls := notifications(w)

		assert.False(t, serveTarget(t, w, rg.pods[0]))
		assert.Equal(t, int32(1), calls.Load())
		assert.Equal(t, podcontract.RestoreReasonSucceeded, podCondition(t, w, "target-0", podcontract.RestoredCondition).Reason)
		assert.Empty(t, k.list())
	})
}

func TestGroupRestoreRecoveryKeepsTheAttemptsDeadline(t *testing.T) {
	for name, tc := range map[string]struct {
		created    time.Duration
		wantCalls  int32
		wantReason string
		wantKills  []int
	}{
		"past the deadline": {
			created: -20 * time.Minute, wantReason: podcontract.RestoreReasonFailed,
			// As without the restart: the action failed, so the member does.
			wantKills: []int{7},
		},
		"within the deadline": {created: -time.Minute, wantCalls: 1, wantReason: podcontract.RestoreReasonSucceeded},
	} {
		t.Run(name, func(t *testing.T) {
			rg := newRestoreGroup(t)
			rg.restore.CreationTimestamp = metav1.NewTime(time.Now().Add(tc.created)) // a 10-minute deadline
			markStarted(rg.pods[0])
			withPostRestore(rg, intstr.FromInt32(8000))
			w, k, s := groupRestoreController(t, nil, rg, (&memberRestore{}).fn)
			s.complete = true
			calls := notifications(w)

			assert.False(t, serveTarget(t, w, rg.pods[0]))

			assert.Equal(t, tc.wantCalls, calls.Load(), "a restart never renews the attempt's budget for the notification")
			restored := podCondition(t, w, "target-0", podcontract.RestoredCondition)
			require.NotNil(t, restored)
			assert.Equal(t, tc.wantReason, restored.Reason)
			assert.Equal(t, tc.wantKills, k.list())
		})
	}
}

// throttleSnapshotReads makes every read of the member's snapshot objects
// fail as a throttled API server's do, and counts them.
func throttleSnapshotReads(w *NodeController) *atomic.Int32 {
	reads := &atomic.Int32{}
	failReads(w, func(obj client.Object) error {
		switch obj.(type) {
		case *snapshotv1alpha1.PodGroupSnapshot, *snapshotv1alpha1.PodSnapshot:
			reads.Add(1)
			return apierrors.NewTooManyRequests("the API server is throttling", 1)
		}
		return nil
	})
	return reads
}

func TestGroupRestoreRecoveryFailsAnUnreleasedMemberWithoutReadingItsSnapshot(t *testing.T) {
	rg := newRestoreGroup(t)
	markStarted(rg.pods[0])
	withPostRestore(rg, intstr.FromInt32(8000))
	w, k, s := groupRestoreController(t, nil, rg, (&memberRestore{}).fn)
	reads := throttleSnapshotReads(w)

	assert.False(t, serveTarget(t, w, rg.pods[0]), "restore-complete decides; nothing waits for the API server")

	restored := podCondition(t, w, "target-0", podcontract.RestoredCondition)
	require.NotNil(t, restored)
	assert.Equal(t, podcontract.RestoreReasonFailed, restored.Reason)
	assert.Contains(t, restored.Message, "before its release")
	assert.Zero(t, reads.Load(), "an unreleased member's snapshot is never read")
	assert.Equal(t, []int{7}, k.list(), "the partly restored workload ends")
	assert.Equal(t, string(restoreFailedSentinelContents), s.get("7/"+podcontract.RestoreFailedFile))
}

func TestGroupRestoreRecoveryStopsWaitingForItsSnapshotAtTheDeadline(t *testing.T) {
	for name, tc := range map[string]struct {
		created    time.Duration
		requeue    bool
		wantReason string
		wantKills  []int
	}{
		"before the deadline": {created: -time.Minute, requeue: true, wantReason: podcontract.RestoreReasonInProgress},
		"past the deadline": {
			created: -20 * time.Minute, wantReason: podcontract.RestoreReasonFailed,
			// As a post-restore action that failed: the workload ends.
			wantKills: []int{7},
		},
	} {
		t.Run(name, func(t *testing.T) {
			rg := newRestoreGroup(t)
			rg.restore.CreationTimestamp = metav1.NewTime(time.Now().Add(tc.created)) // a 10-minute deadline
			markStarted(rg.pods[0])
			withPostRestore(rg, intstr.FromInt32(8000))
			w, k, s := groupRestoreController(t, nil, rg, (&memberRestore{}).fn)
			s.complete = true
			calls := notifications(w)
			reads := throttleSnapshotReads(w)

			assert.Equal(t, tc.requeue, serveTarget(t, w, rg.pods[0]))

			assert.Positive(t, reads.Load(), "a released member reads its snapshot for its post-restore action")
			restored := podCondition(t, w, "target-0", podcontract.RestoredCondition)
			require.NotNil(t, restored)
			assert.Equal(t, tc.wantReason, restored.Reason)
			assert.Zero(t, calls.Load())
			assert.Equal(t, tc.wantKills, k.list())
			if !tc.requeue {
				assert.Contains(t, restored.Message, "deadline")
			}
		})
	}
}

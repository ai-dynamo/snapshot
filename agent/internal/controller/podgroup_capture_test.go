// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/ai-dynamo/snapshot/agent/internal/group"
	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/coordinator"
	"github.com/ai-dynamo/snapshot/coordinator/coordinatortest"
	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/ai-dynamo/snapshot/coordinator/session"
)

const captureGroupUID = types.UID("capture-group-uid")

// captureGroup is a PodGroupSnapshot of two members, leader and worker, each
// with its PodSnapshot, its content on node-a and its source Pod.
type captureGroup struct {
	group    *snapshotv1alpha1.PodGroupSnapshot
	snaps    []*snapshotv1alpha1.PodSnapshot
	contents []*snapshotv1alpha1.PodSnapshotContent
	pods     []*corev1.Pod
}

var captureMemberIDs = []string{"leader", "worker"}

func newCaptureGroup() *captureGroup {
	g := &captureGroup{group: &snapshotv1alpha1.PodGroupSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "pair", Namespace: "inference", UID: captureGroupUID,
			CreationTimestamp: metav1.Now()},
		Spec: snapshotv1alpha1.PodGroupSnapshotSpec{DeadlineSeconds: 600},
	}}
	for rank, id := range captureMemberIDs {
		podName := fmt.Sprintf("worker-%d", rank)
		podUID := types.UID(podName + "-uid")
		g.group.Spec.Members = append(g.group.Spec.Members, snapshotv1alpha1.PodGroupSnapshotMember{
			ID: id, Source: snapshotv1alpha1.PodSnapshotSource{PodRef: snapshotv1alpha1.PodReference{Name: podName, UID: podUID, Containers: []string{"main"}}},
		})
		g.group.Status.Members = append(g.group.Status.Members, snapshotv1alpha1.PodGroupSnapshotMemberStatus{
			ID: id, Rank: int32(rank), NodeName: "node-a",
		})
		snap := &snapshotv1alpha1.PodSnapshot{ObjectMeta: metav1.ObjectMeta{
			Name: "pair-" + id, Namespace: "inference", UID: types.UID("pair-" + id + "-uid"),
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(g.group,
				snapshotv1alpha1.GroupVersion.WithKind(snapshotv1alpha1.KindPodGroupSnapshot))},
		}}
		snap.Spec.PodGroup = &snapshotv1alpha1.PodGroupMembership{Name: g.group.Name, UID: g.group.UID, MemberID: id}
		g.group.Status.Members[rank].PodSnapshot = &snapshotv1alpha1.ObjectIdentity{Name: snap.Name, UID: snap.UID}
		g.snaps = append(g.snaps, snap)
		content := makeWorkOrder("content-"+id, "node-a", id)
		content.Spec.PodSnapshotRef = snapshotv1alpha1.PodSnapshotReference{Namespace: "inference", Name: snap.Name, UID: snap.UID}
		content.Spec.Source.PodRef = snapshotv1alpha1.PodReference{Name: podName, UID: podUID, Containers: []string{"main"}}
		content.Spec.PodGroup = snap.Spec.PodGroup.DeepCopy()
		g.contents = append(g.contents, content)
		pod := makeSourcePod()
		pod.Name, pod.UID = podName, podUID
		pod.Status.PodIP = fmt.Sprintf("10.0.0.%d", rank+1)
		pod.Status.ContainerStatuses[0].ContainerID = "containerd://" + podName
		g.pods = append(g.pods, pod)
	}
	return g
}

func (g *captureGroup) objects() []client.Object {
	objs := []client.Object{g.group}
	for i := range g.snaps {
		objs = append(objs, g.snaps[i], g.contents[i], g.pods[i])
	}
	return objs
}

// kills records the processes a test's controller signals.
type kills struct {
	mu   sync.Mutex
	pids []int
}

func (k *kills) signal(_ logr.Logger, pid int, _ syscall.Signal, _ string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.pids = append(k.pids, pid)
	return nil
}

func (k *kills) list() []int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]int(nil), k.pids...)
}

// groupCaptureController is a controller with pod groups on, whose members
// join srv, and whose dumps checkpointFn runs.
func groupCaptureController(t *testing.T, srv *coordinatortest.Coordinator, g *captureGroup,
	checkpointFn func(context.Context, CheckpointParams) (*pagebroker.PublishedArtifact, error), funcs *interceptor.Funcs,
) (*NodeController, *kills) {
	t.Helper()
	w := makeNodeController(t, &fakeCheckpointer{}, g.objects()...)
	if funcs != nil {
		w.client = interceptor.NewClient(w.client.(client.WithWatch), *funcs)
	}
	w.checkpointFn = checkpointFn
	w.runtime = &fakeRuntime{resolveContainerPID: 7}
	w.log = testr.New(t)
	k := &kills{}
	w.sendSignalFn = k.signal
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
		joinFn: group.Join,
	}
	return w, k
}

// ensureSession starts a coordinator holding the session of an attempt of the
// members, in rank order.
func ensureSession(t *testing.T, uid types.UID, members []string) *coordinatortest.Coordinator {
	t.Helper()
	srv := coordinatortest.Start(t)
	spec := server.Spec{UID: uid, Deadline: time.Now().Add(time.Hour)}
	for rank, id := range members {
		spec.Members = append(spec.Members, session.Member{ID: id, Rank: rank})
	}
	require.NoError(t, srv.Ensure(within(t), spec))
	return srv
}

func within(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// memberCheckpoint stands in for the executor's part in a group capture, as
// executor.Checkpoint plays it: admission, the admitted barrier, the commit
// callback, then the first irreversible step and the net-freeze barrier. A
// member that fails ends the attempt, as the executor does. refuse makes a
// member refuse at admission, panicsBeforeStart makes it panic before it
// starts, and panics makes it panic once it started.
type memberCheckpoint struct {
	t                 *testing.T
	w                 func() *NodeController
	refuse            map[string]bool
	panicsBeforeStart map[string]bool
	panics            map[string]bool
	mu                sync.Mutex
	timeline          []string
}

func (m *memberCheckpoint) record(step string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.timeline = append(m.timeline, step)
}

func (m *memberCheckpoint) steps() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.timeline...)
}

func (m *memberCheckpoint) fn(ctx context.Context, params CheckpointParams) (_ *pagebroker.PublishedArtifact, retErr error) {
	g := params.Group
	if g == nil {
		return nil, errors.New("a member's capture needs its group")
	}
	defer func() {
		if retErr != nil {
			_ = g.Fail(context.WithoutCancel(ctx))
		}
	}()
	id := g.ID()
	if m.refuse[id] {
		return nil, errors.New("the source is not safe to checkpoint")
	}
	if m.panicsBeforeStart[id] {
		panic("the capture panicked before the member started")
	}
	if err := g.Admit(ctx, group.Admission{PodIP: netip.MustParseAddr(params.Pod.Status.PodIP)}); err != nil {
		return nil, err
	}
	if err := g.Barrier(ctx, group.Admitted); err != nil {
		return nil, err
	}
	m.record(fmt.Sprintf("%s admitted, started recorded: %t", id, m.started(params.ContentUID)))
	if err := params.Committed(ctx); err != nil {
		return nil, err
	}
	m.record(fmt.Sprintf("%s committed, started recorded: %t", id, m.started(params.ContentUID)))
	if m.panics[id] {
		panic("the capture panicked after the member started")
	}
	if err := g.Barrier(ctx, group.NetFreeze); err != nil {
		return nil, fmt.Errorf("barrier: %w", err)
	}
	m.record(id + " dumped")
	return nil, nil
}

// started reports whether the content with uid records that its member
// started, as the API server has it.
func (m *memberCheckpoint) started(uid string) bool {
	list := &snapshotv1alpha1.PodSnapshotContentList{}
	require.NoError(m.t, m.w().client.List(context.Background(), list))
	for i := range list.Items {
		if string(list.Items[i].UID) == uid {
			return contentStarted(&list.Items[i])
		}
	}
	return false
}

// captureBoth reconciles both members' contents at once, as the queue's
// per-key goroutines do, and waits for both to return.
func captureBoth(t *testing.T, w *NodeController, g *captureGroup) {
	t.Helper()
	var wg sync.WaitGroup
	for _, content := range g.contents {
		wg.Go(func() {
			assert.NoError(t, w.reconcileCapture(within(t), content.Name))
		})
	}
	wg.Wait()
}

func waitForLeaving(t *testing.T, w *NodeController) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		w.podGroups.leaving.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the members did not leave their attempt")
	}
}

func condition(content *snapshotv1alpha1.PodSnapshotContent, conditionType string) *metav1.Condition {
	return meta.FindStatusCondition(content.Status.Conditions, conditionType)
}

func TestGroupCheckpointOfTwoMembersThroughTheirSession(t *testing.T) {
	g := newCaptureGroup()
	srv := ensureSession(t, captureGroupUID, captureMemberIDs)
	var w *NodeController
	executor := &memberCheckpoint{t: t, w: func() *NodeController { return w }}
	w, k := groupCaptureController(t, srv, g, executor.fn, nil)

	captureBoth(t, w, g)
	waitForLeaving(t, w)

	for i, content := range g.contents {
		got := getContent(t, w, content.Name)
		require.NotNil(t, condition(got, snapshotv1alpha1.PodSnapshotConditionReady), "member %s", captureMemberIDs[i])
		assert.Equal(t, metav1.ConditionTrue, condition(got, snapshotv1alpha1.PodSnapshotConditionReady).Status)
		started := condition(got, snapshotv1alpha1.PodSnapshotConditionStarted)
		require.NotNil(t, started, "the record is kept with Ready")
		assert.Equal(t, metav1.ConditionTrue, started.Status)
	}
	assert.ElementsMatch(t, []string{
		"leader admitted, started recorded: false", "leader committed, started recorded: true", "leader dumped",
		"worker admitted, started recorded: false", "worker committed, started recorded: true", "worker dumped",
	}, executor.steps(), "Started is written between the admitted barrier and the member's next step")
	assert.Empty(t, k.list())
}

func TestGroupCheckpointThatCannotRecordItsStartFailsTheAttempt(t *testing.T) {
	g := newCaptureGroup()
	srv := ensureSession(t, captureGroupUID, captureMemberIDs)
	var w *NodeController
	executor := &memberCheckpoint{t: t, w: func() *NodeController { return w }}
	funcs := &interceptor.Funcs{SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
		if content, ok := obj.(*snapshotv1alpha1.PodSnapshotContent); ok && content.Name == "content-worker" && contentStarted(content) {
			return errors.New("the API server is unavailable")
		}
		return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
	}}
	w, k := groupCaptureController(t, srv, g, executor.fn, funcs)

	captureBoth(t, w, g)

	worker := getContent(t, w, "content-worker")
	assert.Equal(t, reasonCheckpointFailed, condition(worker, snapshotv1alpha1.PodSnapshotConditionFailed).Reason,
		"the member that could not record its start reports its own reason")
	assert.Nil(t, condition(worker, snapshotv1alpha1.PodSnapshotConditionStarted))
	assert.NotContains(t, getPod(t, w, "inference", "worker-1").Labels, snapshotv1alpha1.CaptureEligibleLabel,
		"a member that never started is left as found")

	leader := getContent(t, w, "content-leader")
	assert.Equal(t, session.ReasonMemberFailed, condition(leader, snapshotv1alpha1.PodSnapshotConditionFailed).Reason,
		"the leader reports the attempt's reason")
	assert.NotContains(t, executor.steps(), "leader dumped")
	// The leader may learn that the attempt failed before its admitted barrier
	// returns, and then never start. Both outcomes are legitimate; only a
	// member that started has its source terminated.
	if condition(leader, snapshotv1alpha1.PodSnapshotConditionStarted) != nil {
		assert.Equal(t, []int{7}, k.list(), "only the member that started has its source terminated")
	} else {
		assert.Empty(t, k.list(), "no member started, so no source is touched")
		assert.NotContains(t, getPod(t, w, "inference", "worker-0").Labels, snapshotv1alpha1.CaptureEligibleLabel,
			"a leader that never started is left as found")
	}
}

func TestGroupCheckpointThatPanicsAfterItStartedFailsTheMember(t *testing.T) {
	g := newCaptureGroup()
	srv := ensureSession(t, captureGroupUID, captureMemberIDs)
	var w *NodeController
	executor := &memberCheckpoint{t: t, w: func() *NodeController { return w }, panics: map[string]bool{"leader": true}}
	w, k := groupCaptureController(t, srv, g, executor.fn, nil)

	captureBoth(t, w, g)

	leader := getContent(t, w, "content-leader")
	failed := condition(leader, snapshotv1alpha1.PodSnapshotConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, reasonAgentPanic, failed.Reason)
	assert.Contains(t, failed.Message, "panic")
	assert.NotNil(t, condition(leader, snapshotv1alpha1.PodSnapshotConditionStarted))

	worker := getContent(t, w, "content-worker")
	assert.Equal(t, session.ReasonMemberFailed, condition(worker, snapshotv1alpha1.PodSnapshotConditionFailed).Reason,
		"the attempt fails for the peer")
	// The fake runtime gives both sources PID 7. The worker may learn that
	// the attempt failed before its admitted barrier returns, and then never
	// start; each member that started has its source terminated.
	terminated := 1
	if condition(worker, snapshotv1alpha1.PodSnapshotConditionStarted) != nil {
		terminated = 2
	}
	assert.Equal(t, slices.Repeat([]int{7}, terminated), k.list())
}

func TestGroupCheckpointThatPanicsBeforeItStartedLeavesEveryPodAsFound(t *testing.T) {
	g := newCaptureGroup()
	srv := ensureSession(t, captureGroupUID, captureMemberIDs)
	var w *NodeController
	executor := &memberCheckpoint{t: t, w: func() *NodeController { return w }, panicsBeforeStart: map[string]bool{"leader": true}}
	w, k := groupCaptureController(t, srv, g, executor.fn, nil)

	captureBoth(t, w, g)

	leader := getContent(t, w, "content-leader")
	assert.Equal(t, reasonAgentPanic, condition(leader, snapshotv1alpha1.PodSnapshotConditionFailed).Reason)
	assert.Equal(t, session.ReasonRefused,
		condition(getContent(t, w, "content-worker"), snapshotv1alpha1.PodSnapshotConditionFailed).Reason,
		"the panic fails the attempt before it commits")
	for i, content := range g.contents {
		assert.Nil(t, condition(getContent(t, w, content.Name), snapshotv1alpha1.PodSnapshotConditionStarted))
		assert.NotContains(t, getPod(t, w, "inference", g.pods[i].Name).Labels, snapshotv1alpha1.CaptureEligibleLabel)
	}
	assert.Empty(t, k.list(), "a member that never started has its source left as found")
}

func TestGroupCheckpointThatPanicsAfterItConcludedKeepsItsOutcome(t *testing.T) {
	g := newCaptureGroup()
	srv := ensureSession(t, captureGroupUID, captureMemberIDs)
	var w *NodeController
	executor := &memberCheckpoint{t: t, w: func() *NodeController { return w }}
	var panicking atomic.Bool
	panicking.Store(true)
	funcs := &interceptor.Funcs{SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
		if content, ok := obj.(*snapshotv1alpha1.PodSnapshotContent); ok && content.Name == "content-leader" &&
			isContentReady(content) && panicking.Load() {
			panic("recording Ready panicked")
		}
		return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
	}}
	w, k := groupCaptureController(t, srv, g, executor.fn, funcs)

	errs := make([]error, len(g.contents))
	var wg sync.WaitGroup
	for i, content := range g.contents {
		wg.Go(func() { errs[i] = w.reconcileCapture(within(t), content.Name) })
	}
	wg.Wait()
	waitForLeaving(t, w)

	require.ErrorContains(t, errs[0], "panic", "the outcome that could not be recorded is tried again")
	require.NoError(t, errs[1])
	leader := getContent(t, w, "content-leader")
	assert.Nil(t, condition(leader, snapshotv1alpha1.PodSnapshotConditionFailed), "a capture that concluded is not failed")
	assert.Empty(t, k.list(), "no source is terminated")

	panicking.Store(false)
	require.NoError(t, w.reconcileCapture(within(t), "content-leader"))
	ready := condition(getContent(t, w, "content-leader"), snapshotv1alpha1.PodSnapshotConditionReady)
	require.NotNil(t, ready, "the outcome held is recorded")
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	dumps := 0
	for _, step := range executor.steps() {
		if step == "leader dumped" {
			dumps++
		}
	}
	assert.Equal(t, 1, dumps, "the member is checkpointed once")
}

func TestGroupCheckpointRefusedBeforeItCommitsLeavesEveryPodAsFound(t *testing.T) {
	g := newCaptureGroup()
	srv := ensureSession(t, captureGroupUID, captureMemberIDs)
	var w *NodeController
	executor := &memberCheckpoint{t: t, w: func() *NodeController { return w }, refuse: map[string]bool{"worker": true}}
	w, k := groupCaptureController(t, srv, g, executor.fn, nil)

	captureBoth(t, w, g)

	assert.Equal(t, reasonCheckpointFailed,
		condition(getContent(t, w, "content-worker"), snapshotv1alpha1.PodSnapshotConditionFailed).Reason)
	assert.Equal(t, "Refused",
		condition(getContent(t, w, "content-leader"), snapshotv1alpha1.PodSnapshotConditionFailed).Reason)
	for i, content := range g.contents {
		assert.Nil(t, condition(getContent(t, w, content.Name), snapshotv1alpha1.PodSnapshotConditionStarted))
		assert.NotContains(t, getPod(t, w, "inference", g.pods[i].Name).Labels, snapshotv1alpha1.CaptureEligibleLabel)
	}
	assert.Empty(t, k.list(), "no source is touched")
}

func TestGroupCheckpointAfterARestart(t *testing.T) {
	t.Run("a member that started fails and is never checkpointed again", func(t *testing.T) {
		g := newCaptureGroup()
		meta.SetStatusCondition(&g.contents[0].Status.Conditions, metav1.Condition{
			Type: snapshotv1alpha1.PodSnapshotConditionStarted, Status: metav1.ConditionTrue, Reason: "CheckpointStarted"})
		fc := &fakeCheckpointer{}
		w, k := groupCaptureController(t, nil, g, fc.fn, nil)

		require.NoError(t, w.reconcileCapture(within(t), "content-leader"))

		assert.False(t, fc.wasCalled())
		got := getContent(t, w, "content-leader")
		assert.Equal(t, reasonCheckpointInterrupted, condition(got, snapshotv1alpha1.PodSnapshotConditionFailed).Reason)
		assert.Equal(t, []int{7}, k.list(), "the started member's source is terminated")
	})

	t.Run("a member without the record joins the attempt", func(t *testing.T) {
		g := newCaptureGroup()
		fc := &fakeCheckpointer{}
		w, _ := groupCaptureController(t, nil, g, fc.fn, nil)
		joined := false
		w.podGroups.joinFn = func(context.Context, coordinator.Config) (*group.Member, error) {
			joined = true
			return nil, errors.New("the coordinator refused the connection")
		}
		w.podGroups.configFn = func(context.Context, string, string, group.SessionNode) (coordinator.Config, error) {
			return coordinator.Config{}, nil
		}

		require.NoError(t, w.reconcileCapture(within(t), "content-leader"))
		assert.True(t, joined)
		assert.Equal(t, reasonCoordinatorUnreachable,
			condition(getContent(t, w, "content-leader"), snapshotv1alpha1.PodSnapshotConditionFailed).Reason)
	})

	t.Run("an outcome this agent holds is recorded again", func(t *testing.T) {
		g := newCaptureGroup()
		fc := &fakeCheckpointer{}
		w, _ := groupCaptureController(t, nil, g, fc.fn, nil)
		w.podGroups.captureOutcomes.Store("content-leader", &captureOutcome{
			uid: g.contents[0].UID, reason: session.ReasonMemberFailed, cause: errors.New("a peer failed")})

		require.NoError(t, w.reconcileCapture(within(t), "content-leader"))
		assert.False(t, fc.wasCalled())
		assert.Equal(t, session.ReasonMemberFailed,
			condition(getContent(t, w, "content-leader"), snapshotv1alpha1.PodSnapshotConditionFailed).Reason)
	})
}

func TestGroupCheckpointThatFailsBeforeItJoins(t *testing.T) {
	for name, tc := range map[string]struct {
		// modify sets up the failure, before the controller reads the group.
		modify func(*captureGroup)
		// fail sets it up on the controller.
		fail func(*NodeController)
		want string
	}{
		"the group deadline while waiting for readiness": {
			modify: func(g *captureGroup) {
				g.group.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Minute))
				g.group.Spec.DeadlineSeconds = 1
				g.pods[0].Status.ContainerStatuses[0].Ready = false
				g.pods[0].Labels[snapshotv1alpha1.CaptureEligibleLabel] = "true"
			},
			want: session.ReasonDeadlineExceeded,
		},
		"a misconfigured launcher after the source was labeled": {
			modify: func(g *captureGroup) {
				g.pods[0].Annotations = map[string]string{podcontract.CuInterposeAnnotation: "enabled"}
				g.pods[0].Spec.Containers = []corev1.Container{{Name: "main", Command: []string{"worker"}}}
				g.pods[0].Status.ContainerStatuses[0].Ready = false
				g.pods[0].Labels[snapshotv1alpha1.CaptureEligibleLabel] = "true"
			},
			want: "CuInterposeMisconfigured",
		},
		"a readiness probe that can never run": {
			modify: func(g *captureGroup) {
				g.contents[0].Spec.Containers = []snapshotv1alpha1.ContainerSnapshotConfig{{Name: "main",
					CheckpointReadinessProbe: &snapshotv1alpha1.CheckpointReadinessProbe{PeriodSeconds: 1, TimeoutSeconds: 1, SuccessThreshold: 1}}}
			},
			want: reasonInvalidCheckpointReadinessProbe,
		},
		"a container without an ID": {
			modify: func(g *captureGroup) { g.pods[0].Status.ContainerStatuses[0].ContainerID = "" },
			want:   "ContainerNotResolved",
		},
		"a container that cannot be resolved": {
			fail: func(w *NodeController) { w.runtime = &fakeRuntime{} },
			want: "ContainerNotResolved",
		},
		"an image too large for staging": {
			fail: func(w *NodeController) {
				w.config.PageBroker.StagingPath = "/staging"
				w.stagingCapacityFn = func(string) (int64, error) { return 1 << 30, nil }
				w.minimumImageSizeFn = func(string, []int) (int64, error) { return 4 << 30, nil }
			},
			want: reasonImageTooLarge,
		},
		"a checkpoint container that failed after an earlier reconcile labeled the source": {
			modify: func(g *captureGroup) {
				g.pods[0].Labels[snapshotv1alpha1.CaptureEligibleLabel] = "true"
				g.pods[0].Status.ContainerStatuses[0].State = corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}
			},
			want: "CheckpointContainerFailed",
		},
		"a panic": {
			fail: func(w *NodeController) {
				w.config.PageBroker.StagingPath = "/staging"
				w.stagingCapacityFn = func(string) (int64, error) { panic("staging could not be measured") }
			},
			want: reasonAgentPanic,
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := newCaptureGroup()
			if tc.modify != nil {
				tc.modify(g)
			}
			fc := &fakeCheckpointer{}
			w, k := groupCaptureController(t, nil, g, fc.fn, nil)
			if tc.fail != nil {
				tc.fail(w)
			}
			joined := false
			w.podGroups.joinFn = func(context.Context, coordinator.Config) (*group.Member, error) {
				joined = true
				return nil, errors.New("the member joined")
			}

			w.processCaptureQueueItem(within(t), "content-leader")

			failed := condition(getContent(t, w, "content-leader"), snapshotv1alpha1.PodSnapshotConditionFailed)
			require.NotNil(t, failed, "the member reports its own failure")
			assert.Equal(t, tc.want, failed.Reason)
			assert.False(t, joined, "a member that fails before it joins does not join to fail the attempt")
			assert.False(t, fc.wasCalled())
			assert.NotContains(t, getPod(t, w, "inference", "worker-0").Labels, snapshotv1alpha1.CaptureEligibleLabel,
				"the source is left as found")
			assert.Empty(t, k.list())
		})
	}
}

func TestGroupCheckpointWhoseContainerFailedLeavesTheOtherContainersRunning(t *testing.T) {
	g := newCaptureGroup()
	source := g.pods[0]
	source.Labels[snapshotv1alpha1.CaptureEligibleLabel] = "true"
	source.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}
	source.Status.ContainerStatuses = append(source.Status.ContainerStatuses, corev1.ContainerStatus{
		Name: "sidecar", ContainerID: "containerd://sidecar", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	})
	fc := &fakeCheckpointer{}
	w, k := groupCaptureController(t, nil, g, fc.fn, nil)
	rt := &fakeRuntime{resolveContainerPID: 7}
	w.runtime = rt
	joined := false
	w.podGroups.joinFn = func(context.Context, coordinator.Config) (*group.Member, error) {
		joined = true
		return nil, errors.New("the member joined")
	}

	w.processCaptureQueueItem(within(t), "content-leader")

	assert.Equal(t, "CheckpointContainerFailed",
		condition(getContent(t, w, "content-leader"), snapshotv1alpha1.PodSnapshotConditionFailed).Reason)
	assert.Empty(t, rt.resolvedContainerIDs, "a member's other containers are not swept: its source belongs to its owner")
	assert.Empty(t, k.list())
	assert.NotContains(t, getPod(t, w, "inference", "worker-0").Labels, snapshotv1alpha1.CaptureEligibleLabel,
		"the source is left as found")
	assert.False(t, joined, "a member that fails before it joins does not join to fail the attempt")
	assert.False(t, fc.wasCalled())
}

func TestGroupCheckpointThatCannotJoin(t *testing.T) {
	for name, tc := range map[string]struct {
		deadlineSeconds int64
		want            string
	}{
		"before the deadline": {deadlineSeconds: 600, want: reasonCoordinatorUnreachable},
		"after the deadline":  {deadlineSeconds: 1, want: session.ReasonDeadlineExceeded},
	} {
		t.Run(name, func(t *testing.T) {
			g := newCaptureGroup()
			g.group.Spec.DeadlineSeconds = tc.deadlineSeconds
			g.group.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Minute))
			fc := &fakeCheckpointer{}
			w, k := groupCaptureController(t, nil, g, fc.fn, nil)

			require.NoError(t, w.reconcileCapture(within(t), "content-leader"))
			assert.False(t, fc.wasCalled())
			assert.Equal(t, tc.want, condition(getContent(t, w, "content-leader"), snapshotv1alpha1.PodSnapshotConditionFailed).Reason)
			assert.NotContains(t, getPod(t, w, "inference", "worker-0").Labels, snapshotv1alpha1.CaptureEligibleLabel)
			assert.Empty(t, k.list())
		})
	}
}

func TestGroupCheckpointIsBoundByTheGroupsDeadlineOnly(t *testing.T) {
	g := newCaptureGroup()
	srv := ensureSession(t, captureGroupUID, captureMemberIDs)
	var deadline time.Time
	w, _ := groupCaptureController(t, srv, g, func(ctx context.Context, _ CheckpointParams) (*pagebroker.PublishedArtifact, error) {
		deadline, _ = ctx.Deadline()
		return nil, errors.New("stop once the deadline is read")
	}, nil)
	timeout := 60
	w.config.Checkpoint.TimeoutSeconds = &timeout
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	require.NoError(t, w.reconcileCapture(ctx, "content-leader"))
	assert.WithinDuration(t, g.group.CreationTimestamp.Add(600*time.Second), deadline, time.Second,
		"checkpoint.timeoutSeconds does not bound a member, so it fails as the group does")
}

func TestCaptureMemberOf(t *testing.T) {
	t.Run("a registered member", func(t *testing.T) {
		g := newCaptureGroup()
		w, _ := groupCaptureController(t, nil, g, nil, &interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, readsSnapshot := obj.(*snapshotv1alpha1.PodSnapshot); readsSnapshot {
					t.Fatal("recognition read a PodSnapshot")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
		member, registered, err := w.captureMemberOf(within(t), g.contents[1])
		require.NoError(t, err)
		require.True(t, registered)
		require.NotNil(t, member)
		assert.Equal(t, captureGroupUID, member.attempt)
		assert.Equal(t, "worker", member.id)
		assert.Equal(t, client.ObjectKey{Namespace: "inference", Name: "pair"}, member.group)
		assert.WithinDuration(t, g.group.CreationTimestamp.Add(600*time.Second), member.deadline, time.Second)
		node, err := w.captureSessionNode(member)(within(t))
		require.NoError(t, err)
		assert.Equal(t, "node-a", node)
	})

	t.Run("standalone recognition makes no API read", func(t *testing.T) {
		g := newCaptureGroup()
		g.contents[0].Spec.PodGroup = nil
		var reads int
		w, _ := groupCaptureController(t, nil, g, nil, &interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				reads++
				return c.Get(ctx, key, obj, opts...)
			},
		})
		member, registered, err := w.captureMemberOf(within(t), g.contents[0])
		require.NoError(t, err)
		assert.True(t, registered)
		assert.Nil(t, member)
		assert.Zero(t, reads)
	})

	for _, metadata := range []string{"removed", "changed"} {
		t.Run("child metadata "+metadata+" does not affect recognition", func(t *testing.T) {
			g := newCaptureGroup()
			g.snaps[0].Labels, g.snaps[0].OwnerReferences = nil, nil
			if metadata == "changed" {
				g.snaps[0].Labels = map[string]string{
					"nvidia.com/snapshot-podgroup":        "another",
					"nvidia.com/snapshot-podgroup-member": "stranger",
				}
				g.snaps[0].OwnerReferences = []metav1.OwnerReference{{
					APIVersion: snapshotv1alpha1.GroupVersion.String(), Kind: snapshotv1alpha1.KindPodGroupSnapshot,
					Name: "another", UID: "another-uid",
				}}
			}
			w, _ := groupCaptureController(t, nil, g, nil, nil)
			member, registered, err := w.captureMemberOf(within(t), g.contents[0])
			require.NoError(t, err)
			require.True(t, registered)
			require.NotNil(t, member)
			assert.Equal(t, captureGroupUID, member.attempt)
			assert.Equal(t, captureMemberIDs[0], member.id)
		})
	}

	for name, tc := range map[string]struct {
		modify func(*captureGroup)
		off    bool
		want   string
	}{
		"pod groups off":                      {off: true, want: "pod groups are off"},
		"missing group":                       {modify: func(g *captureGroup) { g.group.Name = "elsewhere" }, want: "is gone"},
		"replaced group":                      {modify: func(g *captureGroup) { g.group.UID = "replacement" }, want: "has UID replacement"},
		"unknown member":                      {modify: func(g *captureGroup) { g.contents[0].Spec.PodGroup.MemberID = "stranger" }, want: "no member"},
		"another source Pod":                  {modify: func(g *captureGroup) { g.group.Spec.Members[0].Source.PodRef.Name = "elsewhere" }, want: "source Pod"},
		"another source UID":                  {modify: func(g *captureGroup) { g.contents[0].Spec.Source.PodRef.UID = "replacement" }, want: "source Pod"},
		"another pinned snapshot":             {modify: func(g *captureGroup) { g.group.Status.Members[0].PodSnapshot.Name = "elsewhere" }, want: "pins PodSnapshot"},
		"same snapshot name with another UID": {modify: func(g *captureGroup) { g.group.Status.Members[0].PodSnapshot.UID = "replacement" }, want: "pins PodSnapshot"},
		"missing snapshot UID":                {modify: func(g *captureGroup) { g.contents[0].Spec.PodSnapshotRef.UID = "" }, want: "no PodSnapshot UID"},
	} {
		t.Run(name, func(t *testing.T) {
			g := newCaptureGroup()
			if tc.modify != nil {
				tc.modify(g)
			}
			fc := &fakeCheckpointer{}
			w, _ := groupCaptureController(t, nil, g, fc.fn, nil)
			if tc.off {
				w.podGroups = nil
			}
			_, _, err := w.captureMemberOf(within(t), g.contents[0])
			require.ErrorContains(t, err, tc.want)
			require.ErrorAs(t, err, new(*invalidMemberError))
			require.NoError(t, w.reconcileCapture(within(t), "content-leader"))
			assert.False(t, fc.wasCalled())
			assert.Equal(t, reasonInvalidPodGroupMember,
				condition(getContent(t, w, "content-leader"), snapshotv1alpha1.PodSnapshotConditionFailed).Reason)
		})
	}
}

func TestCaptureWaitsForItsPinWithoutAnotherObjectEvent(t *testing.T) {
	g := newCaptureGroup()
	pin := g.group.Status.Members[0].PodSnapshot
	g.group.Status.Members[0].PodSnapshot = nil
	g.pods[0].Status.ContainerStatuses[0].Ready = false
	w, _ := groupCaptureController(t, nil, g, nil, nil)
	var delays []time.Duration
	w.requeueCaptureFn = func(name string, after time.Duration) {
		delays = append(delays, after)
		w.captureQueue.Add(name)
	}
	require.NoError(t, w.reconcileCapture(within(t), "content-leader"))
	require.Equal(t, []time.Duration{captureMemberRetryInterval}, delays)
	assert.NotContains(t, getPod(t, w, "inference", "worker-0").Labels, snapshotv1alpha1.CaptureEligibleLabel)

	stored := &snapshotv1alpha1.PodGroupSnapshot{}
	require.NoError(t, w.client.Get(t.Context(), client.ObjectKeyFromObject(g.group), stored))
	stored.Status.Members[0].PodSnapshot = pin
	require.NoError(t, w.client.Update(t.Context(), stored))

	require.Eventually(t, func() bool { return w.captureQueue.Len() > 0 }, time.Second, time.Millisecond)
	name, shutdown := w.captureQueue.Get()
	require.False(t, shutdown)
	require.Equal(t, "content-leader", name)
	require.NoError(t, w.reconcileCapture(within(t), name))
	w.captureQueue.Done(name)
	assert.Equal(t, "true", getPod(t, w, "inference", "worker-0").Labels[snapshotv1alpha1.CaptureEligibleLabel],
		"the scheduled retry advances to readiness without a Pod or content event")
}

func TestCapturePinWaitIsBoundByDeadline(t *testing.T) {
	for _, remaining := range []time.Duration{250 * time.Millisecond, 0} {
		t.Run(remaining.String(), func(t *testing.T) {
			g := newCaptureGroup()
			g.group.CreationTimestamp = metav1.NewTime(time.Now().Truncate(time.Second))
			g.group.Status.Members[0].PodSnapshot = nil
			w, _ := groupCaptureController(t, nil, g, nil, nil)
			deadline := g.group.CreationTimestamp.Add(time.Duration(g.group.Spec.DeadlineSeconds) * time.Second)
			w.nowFn = func() time.Time { return deadline.Add(-remaining) }
			var delays []time.Duration
			w.requeueCaptureFn = func(_ string, after time.Duration) { delays = append(delays, after) }
			require.NoError(t, w.reconcileCapture(within(t), "content-leader"))
			assert.NotContains(t, getPod(t, w, "inference", "worker-0").Labels, snapshotv1alpha1.CaptureEligibleLabel)
			if remaining > 0 {
				assert.Equal(t, []time.Duration{remaining}, delays)
			} else {
				assert.Empty(t, delays)
				assert.Equal(t, session.ReasonDeadlineExceeded,
					condition(getContent(t, w, "content-leader"), snapshotv1alpha1.PodSnapshotConditionFailed).Reason)
			}
		})
	}
}

func TestAttemptEndReason(t *testing.T) {
	deadline := time.Now()
	ended := fmt.Errorf("barrier net-freeze: %w", &session.AbortError{Reason: session.ReasonMemberFailed})
	assert.Equal(t, session.ReasonMemberFailed, attemptEndReason(ended, deadline, deadline.Add(-time.Minute)),
		"a member that failed because the attempt ended reports the attempt's reason")
	assert.Equal(t, session.ReasonCoordinatorLost, attemptEndReason(session.ErrCoordinatorLost, deadline, deadline.Add(-time.Minute)))
	assert.Empty(t, attemptEndReason(errors.New("CRIU failed"), deadline, deadline.Add(-time.Minute)),
		"a member that failed on its own reports its own reason")
	assert.Equal(t, session.ReasonDeadlineExceeded, attemptEndReason(context.DeadlineExceeded, deadline, deadline))
}

func TestDeletedCaptureMemberLeavesSourceAsFound(t *testing.T) {
	for _, finalizing := range []bool{false, true} {
		t.Run(fmt.Sprintf("finalizing-%t", finalizing), func(t *testing.T) {
			g := newCaptureGroup()
			g.pods[0].Status.ContainerStatuses[0].Ready = false
			var objects []runtime.Object
			for _, obj := range g.objects() {
				objects = append(objects, obj)
			}
			w := makeTestController(t, nil, objects...)
			w.config.NodeName = "node-a"
			w.podGroups = &podGroups{}
			content := mustUnstructured(t, g.contents[0])
			content.SetGroupVersionKind(snapshotv1alpha1.GroupVersion.WithKind("PodSnapshotContent"))
			dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
				map[schema.GroupVersionResource]string{
					podSnapshotContentGVR: "PodSnapshotContentList", podGroupRestoreGVR: "PodGroupRestoreList",
				}, content)
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
				return getPod(t, w, "inference", "worker-0").Labels[snapshotv1alpha1.CaptureEligibleLabel] == "true"
			}, 5*time.Second, 10*time.Millisecond, "the member waits at its readiness gate")

			if finalizing {
				content.SetFinalizers([]string{"test.example.com/artifact-cleanup"})
				now := metav1.Now()
				content.SetDeletionTimestamp(&now)
				_, err := dynClient.Resource(podSnapshotContentGVR).Update(ctx, content, metav1.UpdateOptions{})
				require.NoError(t, err)
				require.Eventually(t, func() bool {
					_, labeled := getPod(t, w, "inference", "worker-0").Labels[snapshotv1alpha1.CaptureEligibleLabel]
					return !labeled
				}, 5*time.Second, 10*time.Millisecond, "the deleting update releases the source")
				require.NoError(t, w.client.Delete(ctx, g.snaps[0]))
				require.NoError(t, w.client.Delete(ctx, g.group))
			}

			require.NoError(t, dynClient.Resource(podSnapshotContentGVR).Delete(ctx, content.GetName(), metav1.DeleteOptions{}))
			require.Eventually(t, func() bool {
				_, labeled := getPod(t, w, "inference", "worker-0").Labels[snapshotv1alpha1.CaptureEligibleLabel]
				return !labeled
			}, 5*time.Second, 10*time.Millisecond, "the delete event releases the source label")
		})
	}
}

func TestGroupReadinessWaitRequeuesAtDeadline(t *testing.T) {
	g := newCaptureGroup()
	g.pods[0].Status.ContainerStatuses[0].Ready = false
	fc := &fakeCheckpointer{}
	w, _ := groupCaptureController(t, nil, g, fc.fn, nil)
	now := g.group.CreationTimestamp.Time.Truncate(time.Second)
	w.nowFn = func() time.Time { return now }
	var delay time.Duration
	w.requeueCaptureFn = func(name string, after time.Duration) {
		assert.Equal(t, "content-leader", name)
		delay = after
	}

	require.NoError(t, w.reconcileCapture(within(t), "content-leader"))
	assert.Equal(t, 600*time.Second, delay)
	assert.False(t, fc.wasCalled())
}

func TestCaptureDeleteAfterCacheEviction(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(fmt.Sprintf("started-%t", started), func(t *testing.T) {
			g := newCaptureGroup()
			g.pods[0].Labels[snapshotv1alpha1.CaptureEligibleLabel] = "true"
			if started {
				meta.SetStatusCondition(&g.contents[0].Status.Conditions, metav1.Condition{
					Type: snapshotv1alpha1.PodSnapshotConditionStarted, Status: metav1.ConditionTrue, Reason: "Started",
				})
			}
			w, _ := groupCaptureController(t, nil, g, nil, nil)
			content := mustUnstructured(t, g.contents[0])
			require.NoError(t, w.contentIndexer.Delete(content))
			require.NoError(t, w.client.Delete(t.Context(), g.snaps[0]))
			require.NoError(t, w.client.Delete(t.Context(), g.group))

			require.NoError(t, w.reconcileCapture(t.Context(), content.GetName()))
			w.forgetContent(t.Context(), cache.DeletedFinalStateUnknown{Key: content.GetName(), Obj: content})

			_, labeled := getPod(t, w, "inference", "worker-0").Labels[snapshotv1alpha1.CaptureEligibleLabel]
			assert.Equal(t, started, labeled, "only an unstarted member's source is released")
		})
	}
}

func TestUnstartedMemberCleanupDoesNotNeedItsGroup(t *testing.T) {
	for _, samePod := range []bool{true, false} {
		t.Run(fmt.Sprintf("same-pod-%t", samePod), func(t *testing.T) {
			g := newCaptureGroup()
			g.pods[0].Labels[snapshotv1alpha1.CaptureEligibleLabel] = "true"
			if !samePod {
				g.pods[0].UID = "replacement"
			}
			w, _ := groupCaptureController(t, nil, g, nil, nil)
			require.NoError(t, w.client.Delete(t.Context(), g.group))
			w.leaveUnstartedMemberSource(t.Context(), g.contents[0])
			_, labeled := getPod(t, w, "inference", "worker-0").Labels[snapshotv1alpha1.CaptureEligibleLabel]
			assert.Equal(t, !samePod, labeled, "cleanup only changes the pinned source Pod")
		})
	}
}

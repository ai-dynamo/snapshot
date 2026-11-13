// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/ai-dynamo/snapshot/agent/internal/group"
	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
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
			Labels: map[string]string{snapshotv1alpha1.PodGroupLabel: "pair", snapshotv1alpha1.PodGroupMemberLabel: id},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(g.group,
				snapshotv1alpha1.GroupVersion.WithKind(snapshotv1alpha1.KindPodGroupSnapshot))},
		}}
		g.snaps = append(g.snaps, snap)
		content := makeWorkOrder("content-"+id, "node-a", id)
		content.Spec.PodSnapshotRef = snapshotv1alpha1.PodSnapshotReference{Namespace: "inference", Name: snap.Name, UID: snap.UID}
		content.Spec.Source.PodRef = snapshotv1alpha1.PodReference{Name: podName, UID: podUID, Containers: []string{"main"}}
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
// member refuse at admission.
type memberCheckpoint struct {
	t        *testing.T
	w        func() *NodeController
	refuse   map[string]bool
	mu       sync.Mutex
	timeline []string
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
		"the member that started reports the attempt's reason")
	assert.NotNil(t, condition(leader, snapshotv1alpha1.PodSnapshotConditionStarted))
	assert.Equal(t, []int{7}, k.list(), "only the member that started has its source terminated")
	assert.NotContains(t, executor.steps(), "leader dumped")
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

func TestCaptureMemberOf(t *testing.T) {
	t.Run("a member", func(t *testing.T) {
		g := newCaptureGroup()
		w, _ := groupCaptureController(t, nil, g, nil, nil)
		member, err := w.captureMemberOf(within(t), g.contents[1])
		require.NoError(t, err)
		require.NotNil(t, member)
		assert.Equal(t, captureGroupUID, member.attempt)
		assert.Equal(t, "worker", member.id)
		assert.Equal(t, 1, member.rank)
		assert.Equal(t, client.ObjectKey{Namespace: "inference", Name: "pair"}, member.group)
		assert.WithinDuration(t, g.group.CreationTimestamp.Add(600*time.Second), member.deadline, time.Second)

		node, err := w.captureSessionNode(member)(within(t))
		require.NoError(t, err)
		assert.Equal(t, "node-a", node)
	})

	t.Run("a content captured on its own", func(t *testing.T) {
		g := newCaptureGroup()
		g.snaps[0].Labels, g.snaps[0].OwnerReferences = nil, nil
		w, _ := groupCaptureController(t, nil, g, nil, nil)
		member, err := w.captureMemberOf(within(t), g.contents[0])
		require.NoError(t, err)
		assert.Nil(t, member)
	})

	t.Run("pod groups off", func(t *testing.T) {
		g := newCaptureGroup()
		w, _ := groupCaptureController(t, nil, g, nil, nil)
		w.podGroups = nil
		member, err := w.captureMemberOf(within(t), g.contents[0])
		require.NoError(t, err)
		assert.Nil(t, member, "without pod groups every content is captured on its own")
	})

	for name, tc := range map[string]struct {
		modify func(*captureGroup)
		want   string
	}{
		"no controller": {
			modify: func(g *captureGroup) { g.snaps[0].OwnerReferences = nil },
			want:   "does not control it",
		},
		"another group's UID": {
			modify: func(g *captureGroup) { g.snaps[0].OwnerReferences[0].UID = "another-uid" },
			want:   "not another-uid",
		},
		"no such member": {
			modify: func(g *captureGroup) { g.snaps[0].Labels[snapshotv1alpha1.PodGroupMemberLabel] = "stranger" },
			want:   `no member "stranger"`,
		},
		"another source Pod": {
			modify: func(g *captureGroup) { g.group.Spec.Members[0].Source.PodRef.Name = "elsewhere" },
			want:   "is Pod elsewhere, not worker-0",
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := newCaptureGroup()
			tc.modify(g)
			fc := &fakeCheckpointer{}
			w, _ := groupCaptureController(t, nil, g, fc.fn, nil)

			_, err := w.captureMemberOf(within(t), g.contents[0])
			require.ErrorContains(t, err, tc.want)
			require.ErrorAs(t, err, new(*invalidMemberError))

			require.NoError(t, w.reconcileCapture(within(t), "content-leader"))
			assert.False(t, fc.wasCalled())
			assert.Equal(t, reasonInvalidPodGroupMember,
				condition(getContent(t, w, "content-leader"), snapshotv1alpha1.PodSnapshotConditionFailed).Reason)
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

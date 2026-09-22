// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	snapshottypes "github.com/ai-dynamo/snapshot/agent/internal/types"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// fakeCheckpointer records calls behind the checkpointFn seam and returns a configured error.
type fakeCheckpointer struct {
	mu     sync.Mutex
	called bool
	params CheckpointParams
	err    error
}

// fn is the checkpointFn seam the NodeController invokes for the dump.
func (fc *fakeCheckpointer) fn(_ context.Context, params CheckpointParams) error {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.called = true
	fc.params = params
	return fc.err
}

// wasCalled reports whether the seam was invoked.
func (fc *fakeCheckpointer) wasCalled() bool {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.called
}

// lastParams returns the params from the most recent seam invocation.
func (fc *fakeCheckpointer) lastParams() CheckpointParams {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.params
}

// contentScheme builds a scheme with the PodSnapshotContent and core types registered.
func contentScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, snapshotv1alpha1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

// makeNodeController builds a NodeController wired to a fake typed client, runtime, and seam. Any
// PodSnapshotContent in objs is also added to the podRef index (mirroring the content informer's
// cache) so reconcileCapture can resolve which work order owns the source pod; tests that need a
// different index state override w.contentIndexer after construction.
func makeNodeController(t *testing.T, fc *fakeCheckpointer, objs ...client.Object) *NodeController {
	t.Helper()
	s := contentScheme(t)
	idx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{podRefIndex: podRefIndexFunc})
	for _, o := range objs {
		if sc, ok := o.(*snapshotv1alpha1.PodSnapshotContent); ok {
			require.NoError(t, idx.Add(mustUnstructured(t, sc)))
		}
	}
	w := &NodeController{
		config:    &snapshottypes.AgentConfig{NodeName: "node-a", Storage: snapshottypes.StorageSpec{Type: "pvc", BasePath: t.TempDir()}},
		clientset: k8sfake.NewClientset(),
		client: crfake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
			WithStatusSubresource(&snapshotv1alpha1.PodSnapshotContent{}).Build(),
		runtime:        &fakeRuntime{},
		log:            logr.Discard(),
		contentIndexer: idx,
		captureQueue:   newTestCaptureQueue(t),
	}
	w.checkpointFn = fc.fn
	return w
}

// newTestCaptureQueue builds the capture workqueue with the production rate limiter. Shut down at
// test end so its delaying goroutine does not outlive the test.
func newTestCaptureQueue(t *testing.T) workqueue.TypedRateLimitingInterface[string] {
	t.Helper()
	q := workqueue.NewTypedRateLimitingQueueWithConfig(
		workqueue.DefaultTypedControllerRateLimiter[string](),
		workqueue.TypedRateLimitingQueueConfig[string]{Name: "capture-contents-test"},
	)
	t.Cleanup(q.ShutDown)
	return q
}

// makeWorkOrder builds a PodSnapshotContent work order pinned to a node.
func makeWorkOrder(name, node, suffix string) *snapshotv1alpha1.PodSnapshotContent {
	return &snapshotv1alpha1.PodSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			UID:    types.UID(name + "-uid"),
			Labels: map[string]string{snapshotv1alpha1.SnapshotNodeLabel: node},
		},
		Spec: snapshotv1alpha1.PodSnapshotContentSpec{
			PodSnapshotRef: snapshotv1alpha1.PodSnapshotReference{Namespace: "inference", Name: "podsnapshot-" + suffix},
			Source:         snapshotv1alpha1.PodSnapshotContentSource{PodRef: snapshotv1alpha1.PodReference{Name: "worker-0", UID: types.UID("pod-uid"), Containers: []string{"main"}}, NodeName: node},
		},
	}
}

// makeSourcePod builds a ready source pod with no snapshot annotations. The
// target and artifact identity come from the work order.
func makeSourcePod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "worker-0",
			Namespace: "inference",
			UID:       types.UID("pod-uid"),
			Labels:    map[string]string{},
		},
		Spec: corev1.PodSpec{NodeName: "node-a"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", Ready: true, ContainerID: "containerd://abc123"},
			},
		},
	}
}

// TestSingleTargetContainer asserts the capture read tolerates only exactly one container.
func TestSingleTargetContainer(t *testing.T) {
	cases := []struct {
		name       string
		containers []string
		want       string
		wantErr    bool
	}{
		{name: "exactly one", containers: []string{"main"}, want: "main"},
		{name: "empty", containers: []string{}, wantErr: true},
		{name: "nil", containers: nil, wantErr: true},
		{name: "two", containers: []string{"main", "sidecar"}, wantErr: true},
		{name: "single empty string", containers: []string{""}, wantErr: true},
		{name: "single whitespace", containers: []string{"  "}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content := &snapshotv1alpha1.PodSnapshotContent{
				Spec: snapshotv1alpha1.PodSnapshotContentSpec{
					Source: snapshotv1alpha1.PodSnapshotContentSource{
						PodRef: snapshotv1alpha1.PodReference{Name: "worker-0", Containers: tc.containers},
					},
				},
			}
			got, err := singleTargetContainer(content)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestReconcileCapture_InvalidTargetContainerFails proves the capture path self-defends against a
// work order whose PodReference.Containers violates the exactly-one CRD cap.
func TestReconcileCapture_InvalidTargetContainerFails(t *testing.T) {
	cases := []struct {
		name       string
		containers []string
	}{
		{name: "empty", containers: []string{}},
		{name: "two", containers: []string{"main", "sidecar"}},
		{name: "single blank name", containers: []string{""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")
			content.Spec.Source.PodRef.Containers = tc.containers
			pod := makeSourcePod()
			w := makeNodeController(t, &fakeCheckpointer{}, content, pod)

			require.NoError(t, w.reconcileCapture(context.Background(), content.Name))

			got := getContent(t, w, content.Name)
			cond := meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
			require.NotNil(t, cond)
			assert.Equal(t, metav1.ConditionTrue, cond.Status)
			assert.Equal(t, "InvalidTargetContainer", cond.Reason)
		})
	}
}

// getContent reads a PodSnapshotContent back from the fake client.
func getContent(t *testing.T, w *NodeController, name string) *snapshotv1alpha1.PodSnapshotContent {
	t.Helper()
	c := &snapshotv1alpha1.PodSnapshotContent{}
	require.NoError(t, w.client.Get(context.Background(), types.NamespacedName{Name: name}, c))
	return c
}

// getPod reads a Pod back from the fake client (used to assert CaptureEligibleLabel changes).
func getPod(t *testing.T, w *NodeController, namespace, name string) *corev1.Pod {
	t.Helper()
	p := &corev1.Pod{}
	require.NoError(t, w.client.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, p))
	return p
}

func TestReconcileCapture_IgnoresOtherNode(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-x", "node-b", "x")
	fc := &fakeCheckpointer{}
	w := makeNodeController(t, fc, content)

	require.NoError(t, w.reconcileCapture(context.Background(), content.Name))
	assert.False(t, fc.wasCalled())
	got := getContent(t, w, content.Name)
	assert.Empty(t, got.Status.Conditions)
}

// TestReconcileCapture_PromotesPodAndCaptures covers the happy path: a valid source pod is both
// promoted with CaptureEligibleLabel and dumped in the same pass, because validation and the dump
// are one serialized unit of work.
func TestReconcileCapture_PromotesPodAndCaptures(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")
	pod := makeSourcePod()
	fc := &fakeCheckpointer{}
	w := makeNodeController(t, fc, content, pod)
	w.runtime = &fakeRuntime{resolveContainerPID: 7}

	require.NoError(t, w.reconcileCapture(context.Background(), content.Name))

	assert.True(t, fc.wasCalled())
	assert.Equal(t, "true", getPod(t, w, "inference", "worker-0").Labels[snapshotv1alpha1.CaptureEligibleLabel])
	assert.NotNil(t, meta.FindStatusCondition(
		getContent(t, w, content.Name).Status.Conditions,
		snapshotv1alpha1.PodSnapshotConditionReady,
	))
}

func TestReconcileCapture_DeletingContentDoesNotLabelPod(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")
	now := metav1.Now()
	content.DeletionTimestamp = &now
	content.Finalizers = []string{"test-finalizer"}
	pod := makeSourcePod()
	w := makeNodeController(t, &fakeCheckpointer{}, content, pod)

	require.NoError(t, w.reconcileCapture(context.Background(), content.Name))

	_, labeled := getPod(t, w, "inference", "worker-0").Labels[snapshotv1alpha1.CaptureEligibleLabel]
	assert.False(t, labeled)
	assert.Empty(t, getContent(t, w, content.Name).Status.Conditions)
}

func TestReconcileCapture_DeletingContentDoesNotStartCapture(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")
	now := metav1.Now()
	content.DeletionTimestamp = &now
	content.Finalizers = []string{"test-finalizer"}
	pod := makeSourcePod()
	pod.Labels[snapshotv1alpha1.CaptureEligibleLabel] = "true"
	fc := &fakeCheckpointer{}
	w := makeNodeController(t, fc, content, pod)
	w.runtime = &fakeRuntime{resolveContainerPID: 7}

	require.NoError(t, w.reconcileCapture(context.Background(), content.Name))

	assert.False(t, fc.wasCalled())
	assert.Empty(t, getContent(t, w, content.Name).Status.Conditions)
	assert.Empty(t, w.clientset.(*k8sfake.Clientset).Actions())
}

func TestReconcileCapture_ProvenanceInvalidFailsAndUnlabels(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")
	pod := makeSourcePod()
	pod.UID = types.UID("stale-uid") // UID mismatch vs the work order's pinned source UID
	pod.Labels[snapshotv1alpha1.CaptureEligibleLabel] = "true"
	w := makeNodeController(t, &fakeCheckpointer{}, content, pod)

	require.NoError(t, w.reconcileCapture(context.Background(), content.Name))

	cond := meta.FindStatusCondition(getContent(t, w, content.Name).Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
	require.NotNil(t, cond)
	assert.Equal(t, "StalePodReference", cond.Reason)
	_, labeled := getPod(t, w, "inference", "worker-0").Labels[snapshotv1alpha1.CaptureEligibleLabel]
	assert.False(t, labeled, "eligible label must be removed on cancellation")
}

// TestReconcileCapture_ConcurrentTriggerCannotFailARunningCapture is the regression the capture
// Lease used to cover. The source pod turns terminal the moment the dump kills it, so a trigger
// that lands while the dump is running would read a dead source and write a sticky SourcePodGone.
// The queue is what prevents it: the work order's key is in progress, so that trigger is held and
// only redelivered once the capture has recorded its own outcome.
func TestReconcileCapture_ConcurrentTriggerCannotFailARunningCapture(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	pod := makeSourcePod()
	fc := &fakeCheckpointer{}
	w := makeNodeController(t, fc, content, pod)
	w.runtime = &fakeRuntime{resolveContainerPID: 7}

	dumping := make(chan struct{})
	finish := make(chan struct{})
	w.checkpointFn = func(ctx context.Context, params CheckpointParams) error {
		close(dumping)
		<-finish
		// The dump kills the source: by the time it returns, the pod is terminal.
		terminal := makeSourcePod()
		terminal.Status.Phase = corev1.PodFailed
		require.NoError(t, w.client.Update(ctx, terminal))
		return fc.fn(ctx, params)
	}

	w.captureQueue.Add(content.Name)
	name, _ := w.captureQueue.Get()
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.processCaptureQueueItem(context.Background(), name)
	}()

	<-dumping
	// Both informers fire while the dump runs; neither may reach a reconcile.
	w.enqueueContent(mustUnstructured(t, content))
	w.enqueueCaptureForSourcePod(pod)
	assert.Equal(t, 0, w.captureQueue.Len(), "a work order being captured must not be handed to a second worker")

	close(finish)
	<-done

	got := getContent(t, w, content.Name)
	assert.NotNil(t, meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady))
	assert.Nil(t, meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed))
}

func TestReconcileCapture_FailedContainerUnsticksAndFails(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "worker-0",
			Namespace: "inference",
			UID:       types.UID("pod-uid"),
			Labels:    map[string]string{},
		},
		Spec: corev1.PodSpec{NodeName: "node-a"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, ContainerID: "containerd://main-id"},
				{Name: "helper", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}}, ContainerID: "containerd://helper-id"},
			},
		},
	}
	fc := &fakeCheckpointer{}
	rt := &fakeRuntime{} // PID 0 → ResolveContainer errors → SendSignalToPID skipped (no real signal sent)
	w := makeNodeController(t, fc, content, pod)
	w.runtime = rt

	require.NoError(t, w.reconcileCapture(context.Background(), content.Name))

	got := getContent(t, w, content.Name)
	cond := meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
	require.NotNil(t, cond)
	assert.Equal(t, "CheckpointContainerFailed", cond.Reason)
	assert.Contains(t, cond.Message, "helper")
	assert.True(t, sawEventReason(w.clientset.(*k8sfake.Clientset), "CheckpointFailed"))
	// Only the still-running sibling is resolved for the SIGKILL; the dead container is skipped.
	assert.Equal(t, []string{"main-id"}, rt.resolvedContainerIDs)
	assert.False(t, fc.wasCalled())
}

func TestFailCheckpointOnContainerExit_IgnoresCleanExit(t *testing.T) {
	w := makeNodeController(t, &fakeCheckpointer{})
	pod := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
		{Name: "main", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		{Name: "helper", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
	}}}

	handled := w.failCheckpointOnContainerExit(context.Background(), &snapshotv1alpha1.PodSnapshotContent{}, pod)
	assert.False(t, handled)
}

func TestReconcileCapture_UsesContentUIDArtifactIdentity(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-unrelated-name", "node-a", "abc")
	pod := makeSourcePod()
	fc := &fakeCheckpointer{}
	w := makeNodeController(t, fc, content, pod)
	w.runtime = &fakeRuntime{resolveContainerPID: 7}

	require.NoError(t, w.reconcileCapture(context.Background(), content.Name))

	require.True(t, fc.wasCalled())
	params := fc.lastParams()
	assert.Equal(t, string(content.UID), params.ContentUID)
	assert.Equal(t, filepath.Join(w.config.Storage.BasePath, "artifacts", string(content.UID), "containers", "main"), params.HostPath)
	assert.NotNil(t, meta.FindStatusCondition(
		getContent(t, w, content.Name).Status.Conditions,
		snapshotv1alpha1.PodSnapshotConditionReady,
	))
}

func TestReconcileCapture_ResumeWritesReady(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	pod := makeSourcePod()
	fc := &fakeCheckpointer{}
	w := makeNodeController(t, fc, content, pod)
	// Pre-create the artifact directory at the resolved destination so the resume check fires.
	dest := filepath.Join(w.config.Storage.BasePath, "artifacts", string(content.UID), "containers", "main")
	require.NoError(t, os.MkdirAll(dest, 0o755))
	require.NoError(t, snapshottypes.WriteManifest(dest, &snapshottypes.CheckpointManifest{Artifact: snapshottypes.ArtifactManifest{ContentUID: string(content.UID), ContainerName: "main"}}))

	require.NoError(t, w.reconcileCapture(context.Background(), content.Name))
	assert.False(t, fc.wasCalled())
	got := getContent(t, w, content.Name)
	cond := meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady)
	require.NotNil(t, cond)
}

// TestReconcileCapture_ArtifactRecoveryPrecedesLivenessFailures covers the crash window:
// the dump committed the artifact and terminated the source (pod Failed, target container
// exited 137, PID unresolvable), but the agent died before the Ready write. Recovery must
// mark Ready instead of tripping any liveness-derived failure.
func TestReconcileCapture_ArtifactRecoveryPrecedesLivenessFailures(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	pod := makeSourcePod()
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "main",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "Error"}},
	}}
	fc := &fakeCheckpointer{}
	rt := &fakeRuntime{} // PID 0 → ResolveContainer would error if reached
	w := makeNodeController(t, fc, content, pod)
	w.runtime = rt
	dest := filepath.Join(w.config.Storage.BasePath, "artifacts", string(content.UID), "containers", "main")
	require.NoError(t, os.MkdirAll(dest, 0o755))
	require.NoError(t, snapshottypes.WriteManifest(dest, &snapshottypes.CheckpointManifest{Artifact: snapshottypes.ArtifactManifest{ContentUID: string(content.UID), ContainerName: "main"}}))

	require.NoError(t, w.reconcileCapture(context.Background(), content.Name))

	assert.False(t, fc.wasCalled())
	assert.Empty(t, rt.resolvedContainerIDs, "recovery must not resolve or signal the dead container")
	got := getContent(t, w, content.Name)
	assert.NotNil(t, meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady))
	assert.Nil(t, meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed))
}

// TestReconcileCapture_TerminalPodWithoutArtifactFails proves the artifact-first ordering
// does not swallow genuine failures: a dead source with no committed artifact stays terminal.
func TestReconcileCapture_TerminalPodWithoutArtifactFails(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	pod := makeSourcePod()
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses = nil
	w := makeNodeController(t, &fakeCheckpointer{}, content, pod)

	require.NoError(t, w.reconcileCapture(context.Background(), content.Name))

	got := getContent(t, w, content.Name)
	cond := meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
	require.NotNil(t, cond)
	assert.Equal(t, "SourcePodGone", cond.Reason)
}

// TestReconcileCapture_ReadyDoesNotStarveNewDump proves a finished work order does not keep
// ownership of its source pod: the newer pending one is free to dump.
func TestReconcileCapture_ReadyDoesNotStarveNewDump(t *testing.T) {
	ready := makeWorkOrder("podsnapshotcontent-old", "node-a", "abc")
	ready.CreationTimestamp = metav1.Unix(1000, 0)
	meta.SetStatusCondition(&ready.Status.Conditions, metav1.Condition{
		Type:    snapshotv1alpha1.PodSnapshotConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  "Captured",
		Message: "Checkpoint captured and verified",
	})
	pending := makeWorkOrder("podsnapshotcontent-new", "node-a", "abc")
	pending.CreationTimestamp = metav1.Unix(2000, 0)
	pod := makeSourcePod()
	fc := &fakeCheckpointer{}
	w := makeNodeController(t, fc, ready, pending, pod)
	w.runtime = &fakeRuntime{resolveContainerPID: 7}

	require.NoError(t, w.reconcileCapture(context.Background(), pending.Name))
	assert.True(t, fc.wasCalled())
}

func TestReconcileCapture_InvalidReadySpecDoesNotStarveNewDump(t *testing.T) {
	ready := makeWorkOrder("podsnapshotcontent-old", "node-a", "abc")
	ready.CreationTimestamp = metav1.Unix(1000, 0)
	ready.Spec.Source.PodRef.Containers = nil
	meta.SetStatusCondition(&ready.Status.Conditions, metav1.Condition{
		Type:    snapshotv1alpha1.PodSnapshotConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  "Captured",
		Message: "Checkpoint captured and verified",
	})
	pending := makeWorkOrder("podsnapshotcontent-new", "node-a", "abc")
	pending.CreationTimestamp = metav1.Unix(2000, 0)
	pod := makeSourcePod()
	fc := &fakeCheckpointer{}
	w := makeNodeController(t, fc, ready, pending, pod)
	w.runtime = &fakeRuntime{resolveContainerPID: 7}

	require.NoError(t, w.reconcileCapture(context.Background(), pending.Name))
	assert.True(t, fc.wasCalled())
}

// TestReconcileCapture_OlderWorkOrderOwnsTheSourcePod pins the one-capture-per-pod rule: keys are
// per work order, so the newer sibling must stand down rather than dump the same container.
func TestReconcileCapture_OlderWorkOrderOwnsTheSourcePod(t *testing.T) {
	older := makeWorkOrder("podsnapshotcontent-old", "node-a", "abc")
	older.CreationTimestamp = metav1.Unix(1000, 0)
	newer := makeWorkOrder("podsnapshotcontent-new", "node-a", "abc")
	newer.CreationTimestamp = metav1.Unix(2000, 0)
	pod := makeSourcePod()
	fc := &fakeCheckpointer{}
	w := makeNodeController(t, fc, older, newer, pod)
	w.runtime = &fakeRuntime{resolveContainerPID: 7}

	require.NoError(t, w.reconcileCapture(context.Background(), newer.Name))

	assert.False(t, fc.wasCalled(), "the newer work order must not dump while an older one is active")
	assert.Empty(t, getContent(t, w, newer.Name).Status.Conditions)
}

func TestReconcileCapture_ReadyContentIsNoOp(t *testing.T) {
	ready := makeWorkOrder("podsnapshotcontent-ready", "node-a", "abc")
	meta.SetStatusCondition(&ready.Status.Conditions, metav1.Condition{
		Type:    snapshotv1alpha1.PodSnapshotConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  "Captured",
		Message: "Checkpoint captured and verified",
	})
	pod := makeSourcePod()
	fc := &fakeCheckpointer{}
	w := makeNodeController(t, fc, ready, pod)

	// A Ready work order is terminal: nothing to dump, nothing to release —
	// the dump already terminated the source process.
	require.NoError(t, w.reconcileCapture(context.Background(), ready.Name))
	assert.False(t, fc.wasCalled())
}

func TestReconcileCapture_PodNotFoundFails(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")
	w := makeNodeController(t, &fakeCheckpointer{}, content) // no pod

	require.NoError(t, w.reconcileCapture(context.Background(), content.Name))
	got := getContent(t, w, content.Name)
	cond := meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
	require.NotNil(t, cond)
	assert.Equal(t, "SourcePodNotFound", cond.Reason)
}

func TestClassifySourcePod(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-x", "node-a", "x") // PodRef Name worker-0, UID pod-uid
	running := func(uid string, phase corev1.PodPhase, deleting bool) *corev1.Pod {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Namespace: "inference", UID: types.UID(uid)},
			Status:     corev1.PodStatus{Phase: phase},
		}
		if deleting {
			now := metav1.Now()
			p.DeletionTimestamp = &now
		}
		return p
	}

	reason, _ := classifySourcePodIdentity(content, running("pod-uid", corev1.PodRunning, false))
	assert.Equal(t, "", reason)

	reason, _ = classifySourcePodIdentity(content, running("other-uid", corev1.PodRunning, false))
	assert.Equal(t, "StalePodReference", reason)

	reason, _ = classifySourcePodLiveness(running("pod-uid", corev1.PodRunning, false))
	assert.Equal(t, "", reason)

	for _, phase := range []corev1.PodPhase{corev1.PodFailed, corev1.PodSucceeded} {
		reason, _ = classifySourcePodLiveness(running("pod-uid", phase, false))
		assert.Equal(t, "SourcePodGone", reason)
	}

	reason, _ = classifySourcePodLiveness(running("pod-uid", corev1.PodRunning, true))
	assert.Equal(t, "SourcePodGone", reason)
}

func TestReconcileCapture_StalePodUIDFails(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Namespace: "inference", UID: types.UID("different-uid")},
		Spec:       corev1.PodSpec{NodeName: "node-a"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	w := makeNodeController(t, &fakeCheckpointer{}, content, pod)

	require.NoError(t, w.reconcileCapture(context.Background(), content.Name))
	got := getContent(t, w, content.Name)
	cond := meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
	require.NotNil(t, cond)
	assert.Equal(t, "StalePodReference", cond.Reason)
}

func TestReconcileCapture_PodFailedFails(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Namespace: "inference", UID: types.UID("pod-uid")},
		Spec:       corev1.PodSpec{NodeName: "node-a"},
		Status:     corev1.PodStatus{Phase: corev1.PodFailed},
	}
	w := makeNodeController(t, &fakeCheckpointer{}, content, pod)

	require.NoError(t, w.reconcileCapture(context.Background(), content.Name))
	got := getContent(t, w, content.Name)
	cond := meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
	require.NotNil(t, cond)
	assert.Equal(t, "SourcePodGone", cond.Reason)
}

func TestReconcileCapture_NotReadyQuiesceNoOp(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")
	pod := makeSourcePod()
	pod.Status.ContainerStatuses[0].Ready = false
	fc := &fakeCheckpointer{}
	w := makeNodeController(t, fc, content, pod)

	require.NoError(t, w.reconcileCapture(context.Background(), content.Name))
	assert.False(t, fc.wasCalled())
	got := getContent(t, w, content.Name)
	assert.Empty(t, got.Status.Conditions)
}

func TestReconcileCapture_CapturesFromPod(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	pod := makeSourcePod()
	fc := &fakeCheckpointer{}
	w := makeNodeController(t, fc, content, pod)
	w.runtime = &fakeRuntime{resolveContainerPID: 7}

	require.NoError(t, w.reconcileCapture(context.Background(), content.Name))

	require.True(t, fc.wasCalled())
	params := fc.lastParams()
	assert.Equal(t, string(content.UID), params.ContentUID)
	assert.Equal(t, "main", params.ContainerName)
	assert.Equal(t, "abc123", params.ContainerID)
	assert.Equal(t, 7, params.ContainerPID)
	dest := filepath.Join(w.config.Storage.BasePath, "artifacts", string(content.UID), "containers", "main")
	assert.Equal(t, dest, params.HostPath)
	assert.NotNil(t, meta.FindStatusCondition(
		getContent(t, w, content.Name).Status.Conditions,
		snapshotv1alpha1.PodSnapshotConditionReady,
	))
}

func TestRunCheckpoint_WritesReady(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	fc := &fakeCheckpointer{}
	w := makeNodeController(t, fc, content)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Namespace: "inference", UID: types.UID("pod-uid")}}
	artifactPath := filepath.Join(w.config.Storage.BasePath, "artifacts", string(content.UID), "containers", "main")

	require.NoError(t, w.runCheckpoint(context.Background(), content, pod, "main", "abc123", 7, string(content.UID), artifactPath))

	assert.True(t, fc.wasCalled())
	require.NotNil(t, meta.FindStatusCondition(
		getContent(t, w, content.Name).Status.Conditions,
		snapshotv1alpha1.PodSnapshotConditionReady,
	), "a successful dump must end with a durable Ready condition")
}

func TestRunCheckpoint_WritesFailedOnError(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	fc := &fakeCheckpointer{err: errors.New("criu boom")}
	w := makeNodeController(t, fc, content)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Namespace: "inference", UID: types.UID("pod-uid")}}
	artifactPath := filepath.Join(w.config.Storage.BasePath, "artifacts", string(content.UID), "containers", "main")

	// A recorded failure is a settled outcome, so the queue is told not to retry it.
	require.NoError(t, w.runCheckpoint(context.Background(), content, pod, "main", "abc123", 7, string(content.UID), artifactPath))

	got := getContent(t, w, content.Name)
	cond := meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
	require.NotNil(t, cond)
	assert.Equal(t, "CheckpointFailed", cond.Reason)
}

func TestExecutorCheckpointPageBrokerPrepareFailureDoesNotKill(t *testing.T) {
	w := makeNodeController(t, &fakeCheckpointer{})
	w.config.PageBroker = snapshottypes.PageBrokerSpec{
		Enabled:           true,
		ControlSocketPath: filepath.Join(t.TempDir(), "pagebroker.sock"),
	}
	ctx, target := startKillableTarget(t)
	defer func() {
		_ = target.Process.Kill()
		_ = target.Wait()
	}()

	err := w.executorCheckpoint(context.Background(), CheckpointParams{
		Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name:      "worker-0",
			Namespace: "inference",
			Annotations: map[string]string{
				snapshotv1alpha1.PageBrokerAnnotation: snapshotv1alpha1.PageBrokerAnnotationEnabled,
			},
		}},
		ContainerName: "main",
		ContainerID:   "abc123",
		ContainerPID:  target.Process.Pid,
		ContentUID:    "content-uid",
	})
	require.ErrorContains(t, err, "prepare PageBroker checkpoint")
	require.NoError(t, target.Process.Signal(syscall.Signal(0)), "PageBroker preflight failure must not kill the source")
	require.NoError(t, ctx.Err())
}

// TestReconcileCapture_TerminalPodWithArtifactRecoversReady covers a resync landing after a
// capture that the agent did not live to finish recording: the pod is already terminal (killed by
// the dump) and the artifact is committed, so this must recover Ready, not write SourcePodGone.
func TestReconcileCapture_TerminalPodWithArtifactRecoversReady(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	pod := makeSourcePod()
	pod.Status.Phase = corev1.PodFailed
	w := makeNodeController(t, &fakeCheckpointer{}, content, pod)
	dest := filepath.Join(w.config.Storage.BasePath, "artifacts", string(content.UID), "containers", "main")
	require.NoError(t, os.MkdirAll(dest, 0o755))
	require.NoError(t, snapshottypes.WriteManifest(dest, &snapshottypes.CheckpointManifest{Artifact: snapshottypes.ArtifactManifest{ContentUID: string(content.UID), ContainerName: "main"}}))

	require.NoError(t, w.reconcileCapture(context.Background(), content.Name))

	got := getContent(t, w, content.Name)
	assert.NotNil(t, meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady))
	assert.Nil(t, meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed))
}

// TestReconcileCapture_PodNotFoundWithArtifactRecoversReady covers the source pod being deleted
// (not just terminal) after the dump committed the artifact but before the Ready write landed:
// recovery must mark Ready instead of writing SourcePodNotFound.
func TestReconcileCapture_PodNotFoundWithArtifactRecoversReady(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	w := makeNodeController(t, &fakeCheckpointer{}, content) // no pod
	dest := filepath.Join(w.config.Storage.BasePath, "artifacts", string(content.UID), "containers", "main")
	require.NoError(t, os.MkdirAll(dest, 0o755))
	require.NoError(t, snapshottypes.WriteManifest(dest, &snapshottypes.CheckpointManifest{Artifact: snapshottypes.ArtifactManifest{ContentUID: string(content.UID), ContainerName: "main"}}))

	require.NoError(t, w.reconcileCapture(context.Background(), content.Name))

	got := getContent(t, w, content.Name)
	assert.NotNil(t, meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady))
	assert.Nil(t, meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed))
}

// startKillableTarget starts a short-lived sleep process the test can assert was left alone.
func startKillableTarget(t *testing.T) (context.Context, *exec.Cmd) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	target := exec.CommandContext(ctx, "sleep", "30")
	require.NoError(t, target.Start())
	t.Cleanup(func() {
		if target.ProcessState == nil {
			_ = target.Process.Kill()
		}
	})
	return ctx, target
}

// mustUnstructured converts a typed object to the *unstructured.Unstructured the dynamic informer
// (and thus the podRef index) stores.
func mustUnstructured(t *testing.T, obj runtime.Object) *unstructured.Unstructured {
	t.Helper()
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	require.NoError(t, err)
	return &unstructured.Unstructured{Object: m}
}

// contentForWorker0 builds a PodSnapshotContent referencing pod inference/worker-0 with a given
// creation time, optionally carrying a terminal condition (PodSnapshotConditionReady/Failed).
func contentForWorker0(name string, created metav1.Time, terminal string) *snapshotv1alpha1.PodSnapshotContent {
	c := &snapshotv1alpha1.PodSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: created},
		Spec: snapshotv1alpha1.PodSnapshotContentSpec{
			PodSnapshotRef: snapshotv1alpha1.PodSnapshotReference{Namespace: "inference", Name: "snapshot-" + name},
			Source:         snapshotv1alpha1.PodSnapshotContentSource{PodRef: snapshotv1alpha1.PodReference{Name: "worker-0", UID: types.UID("pod-uid")}, NodeName: "node-a"},
		},
	}
	if terminal != "" {
		meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{Type: terminal, Status: metav1.ConditionTrue, Reason: "Done"})
	}
	return c
}

func TestPodRefIndexFunc(t *testing.T) {
	keys, err := podRefIndexFunc(mustUnstructured(t, contentForWorker0("podsnapshotcontent-abc", metav1.Unix(1000, 0), "")))
	require.NoError(t, err)
	assert.Equal(t, []string{"inference/worker-0"}, keys)
}

func TestPodRefIndexFunc_MissingFieldsOrWrongType(t *testing.T) {
	keys, err := podRefIndexFunc(&unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{}}})
	require.NoError(t, err)
	assert.Nil(t, keys)

	keys, err = podRefIndexFunc("not-unstructured")
	require.NoError(t, err)
	assert.Nil(t, keys)
}

func TestContentFromInformerObj(t *testing.T) {
	u := mustUnstructured(t, contentForWorker0("podsnapshotcontent-abc", metav1.Unix(1000, 0), ""))

	c, ok := contentFromInformerObj(u)
	require.True(t, ok)
	assert.Equal(t, "podsnapshotcontent-abc", c.Name)

	c, ok = contentFromInformerObj(cache.DeletedFinalStateUnknown{Key: "k", Obj: u})
	require.True(t, ok)
	assert.Equal(t, "podsnapshotcontent-abc", c.Name)

	_, ok = contentFromInformerObj(cache.DeletedFinalStateUnknown{Key: "k", Obj: "bad"})
	assert.False(t, ok)
	_, ok = contentFromInformerObj("bad")
	assert.False(t, ok)
}

func TestChooseActiveContent_OldestNonTerminalWins(t *testing.T) {
	// "podsnapshotcontent-a" sorts first by name but is newer; oldest-by-CreationTimestamp must win.
	newer := mustUnstructured(t, contentForWorker0("podsnapshotcontent-a", metav1.Unix(2000, 0), ""))
	older := mustUnstructured(t, contentForWorker0("podsnapshotcontent-b", metav1.Unix(1000, 0), ""))
	assert.Equal(t, "podsnapshotcontent-b", chooseActiveContent([]interface{}{newer, older}))
}

func TestChooseActiveContent_SkipsTerminalAndTieBreaksByName(t *testing.T) {
	terminal := mustUnstructured(t, contentForWorker0("podsnapshotcontent-old", metav1.Unix(1000, 0), snapshotv1alpha1.PodSnapshotConditionReady))
	tieA := mustUnstructured(t, contentForWorker0("podsnapshotcontent-a", metav1.Unix(2000, 0), ""))
	tieB := mustUnstructured(t, contentForWorker0("podsnapshotcontent-b", metav1.Unix(2000, 0), ""))
	assert.Equal(t, "podsnapshotcontent-a", chooseActiveContent([]interface{}{terminal, tieB, tieA}))
}

func TestChooseActiveContent_SkipsDeletingContent(t *testing.T) {
	deletingContent := contentForWorker0("podsnapshotcontent-old", metav1.Unix(1000, 0), "")
	now := metav1.Now()
	deletingContent.DeletionTimestamp = &now
	activeContent := mustUnstructured(t, contentForWorker0("podsnapshotcontent-new", metav1.Unix(2000, 0), ""))

	assert.Equal(t, "podsnapshotcontent-new", chooseActiveContent([]interface{}{mustUnstructured(t, deletingContent), activeContent}))
}

func TestChooseActiveContent_AllTerminalReturnsEmpty(t *testing.T) {
	ready := mustUnstructured(t, contentForWorker0("podsnapshotcontent-a", metav1.Unix(1000, 0), snapshotv1alpha1.PodSnapshotConditionReady))
	failed := mustUnstructured(t, contentForWorker0("podsnapshotcontent-b", metav1.Unix(2000, 0), snapshotv1alpha1.PodSnapshotConditionFailed))
	assert.Equal(t, "", chooseActiveContent([]interface{}{ready, failed}))
}

// podWithFailedSibling builds the inference/worker-0 source pod with the target Running and a
// sibling Terminated non-zero, so a reconcile triggers the unstick.
func podWithFailedSibling() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "worker-0",
			Namespace: "inference",
			UID:       types.UID("pod-uid"),
			Labels:    map[string]string{},
		},
		Spec: corev1.PodSpec{NodeName: "node-a"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, ContainerID: "containerd://main-id"},
				{Name: "helper", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}}, ContainerID: "containerd://helper-id"},
			},
		},
	}
}

func seedIndex(t *testing.T, contents ...*snapshotv1alpha1.PodSnapshotContent) cache.Indexer {
	t.Helper()
	idx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{podRefIndex: podRefIndexFunc})
	for _, c := range contents {
		require.NoError(t, idx.Add(mustUnstructured(t, c)))
	}
	return idx
}

func TestReconcileCapture_TriggersUnstick(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	content.CreationTimestamp = metav1.Unix(1000, 0)
	pod := podWithFailedSibling()
	fc := &fakeCheckpointer{}
	rt := &fakeRuntime{}
	w := makeNodeController(t, fc, content, pod)
	w.runtime = rt

	require.NoError(t, w.reconcileCapture(context.Background(), content.Name))

	got := getContent(t, w, content.Name)
	cond := meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
	require.NotNil(t, cond)
	assert.Equal(t, "CheckpointContainerFailed", cond.Reason)
	assert.Equal(t, []string{"main-id"}, rt.resolvedContainerIDs)
	assert.False(t, fc.wasCalled())
}

// TestReconcileCapture_PodNotIndexedNoOp guards the fail-closed side of source-pod ownership: with
// nothing indexed for the pod there is no owner, so no dump may start.
func TestReconcileCapture_PodNotIndexedNoOp(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	pod := makeSourcePod()
	fc := &fakeCheckpointer{}
	w := makeNodeController(t, fc, content, pod)
	w.runtime = &fakeRuntime{resolveContainerPID: 7}
	w.contentIndexer = seedIndex(t) // override: empty index

	require.NoError(t, w.reconcileCapture(context.Background(), content.Name))

	assert.False(t, fc.wasCalled())
	assert.Empty(t, getContent(t, w, content.Name).Status.Conditions)
}

func TestReconcileCapture_IndexErrorReturned(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	pod := makeSourcePod()
	fc := &fakeCheckpointer{}
	w := makeNodeController(t, fc, content, pod)
	w.runtime = &fakeRuntime{resolveContainerPID: 7}
	// Indexer without podRefIndex registered → ByIndex returns an error; reconcile surfaces it
	// so the queue retries, and writes no status.
	w.contentIndexer = cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})

	require.Error(t, w.reconcileCapture(context.Background(), content.Name))

	assert.False(t, fc.wasCalled())
	assert.Empty(t, getContent(t, w, content.Name).Status.Conditions)
}

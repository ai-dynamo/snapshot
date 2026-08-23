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
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
// cache) so the pod-driven reconcileSourcePod can resolve it; tests that need a different index
// state override w.contentIndexer after construction.
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
		holderID:       "snapshot-agent/test",
		inFlight:       make(map[string]struct{}),
		contentIndexer: idx,
	}
	w.checkpointFn = fc.fn
	w.releaseCheckpointFn = func(int) error { return nil }
	return w
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

// TestReconcileSourcePod_InvalidTargetContainerFails proves the capture path self-defends against a
// work order whose PodReference.Containers violates the exactly-one CRD cap.
func TestReconcileSourcePod_InvalidTargetContainerFails(t *testing.T) {
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

			require.NoError(t, w.reconcileSourcePod(context.Background(), pod))

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

func TestReconcileSnapshotContent_IgnoresOtherNode(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-x", "node-b", "x")
	fc := &fakeCheckpointer{}
	w := makeNodeController(t, fc, content)

	w.reconcilePodSnapshotContent(context.Background(), content.Name)
	assert.False(t, fc.wasCalled())
	got := getContent(t, w, content.Name)
	assert.Empty(t, got.Status.Conditions)
}

func TestReconcileSnapshotContent_GateLabelsPodOnSuccess(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")
	pod := makeSourcePod()
	fc := &fakeCheckpointer{}
	w := makeNodeController(t, fc, content, pod)

	// The gate promotes a valid pod by labeling it; it must NOT run the capture flow itself.
	w.reconcilePodSnapshotContent(context.Background(), content.Name)

	assert.False(t, fc.wasCalled(), "gate must not invoke the dump directly")
	assert.Equal(t, "true", getPod(t, w, "inference", "worker-0").Labels[snapshotv1alpha1.CaptureEligibleLabel])
	assert.Empty(t, getContent(t, w, content.Name).Status.Conditions)
}

func TestReconcileSourcePod_InFlightGuard(t *testing.T) {
	// The UID is unrelated to the content name, proving the guard is keyed on the
	// immutable content UID and target container, not the content name.
	content := makeWorkOrder("podsnapshotcontent-mywork", "node-a", "x")
	content.UID = types.UID("unrelated-content-uid")
	pod := makeSourcePod()
	pod.Labels[snapshotv1alpha1.CaptureEligibleLabel] = "true"
	w := makeNodeController(t, &fakeCheckpointer{}, content, pod)
	w.inFlight[string(content.UID)+"/main"] = struct{}{}

	require.NoError(t, w.reconcileSourcePod(context.Background(), pod))
	got := getContent(t, w, content.Name)
	assert.Empty(t, got.Status.Conditions, "in-flight guard must not write any status")
}

func TestReconcileSourcePod_ProvenanceInvalidFailsAndUnlabels(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")
	pod := makeSourcePod()
	pod.UID = types.UID("stale-uid") // UID mismatch vs the work order's pinned source UID
	pod.Labels[snapshotv1alpha1.CaptureEligibleLabel] = "true"
	w := makeNodeController(t, &fakeCheckpointer{}, content, pod)

	require.NoError(t, w.reconcileSourcePod(context.Background(), pod))

	cond := meta.FindStatusCondition(getContent(t, w, content.Name).Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
	require.NotNil(t, cond)
	assert.Equal(t, "StalePodReference", cond.Reason)
	_, labeled := getPod(t, w, "inference", "worker-0").Labels[snapshotv1alpha1.CaptureEligibleLabel]
	assert.False(t, labeled, "eligible label must be removed on cancellation")
}

func TestReconcileSourcePod_InFlightShortCircuits(t *testing.T) {
	// The guard is keyed on the immutable content UID and container.
	content := makeWorkOrder("podsnapshotcontent-mywork", "node-a", "x")
	pod := makeSourcePod()
	pod.Labels[snapshotv1alpha1.CaptureEligibleLabel] = "true"
	w := makeNodeController(t, &fakeCheckpointer{}, content, pod)
	// A dump is already in flight: tryAcquire short-circuits before any further work, so a second
	// reconcile does nothing — no status write, no relabel.
	w.inFlight[string(content.UID)+"/main"] = struct{}{}

	require.NoError(t, w.reconcileSourcePod(context.Background(), pod))

	got := getPod(t, w, "inference", "worker-0")
	assert.Empty(t, getContent(t, w, content.Name).Status.Conditions, "in-flight dump must not be touched")
	assert.Equal(t, "true", got.Labels[snapshotv1alpha1.CaptureEligibleLabel], "in-flight dump must not be unlabeled")
}

func TestReconcileSnapshotContent_FailedContainerUnsticksAndFails(t *testing.T) {
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

	require.NoError(t, w.reconcileSourcePod(context.Background(), pod))

	got := getContent(t, w, content.Name)
	cond := meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
	require.NotNil(t, cond)
	assert.Equal(t, "CheckpointContainerFailed", cond.Reason)
	assert.Contains(t, cond.Message, "helper")
	assert.True(t, sawEventReason(w.clientset.(*k8sfake.Clientset), "CheckpointFailed"))
	// Only the still-running sibling is resolved for the SIGKILL; the dead container is skipped.
	assert.Equal(t, []string{"main-id"}, rt.resolvedContainerIDs)
	assert.False(t, fc.wasCalled())
	assert.Empty(t, w.inFlight)
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

func TestReconcileSnapshotContent_UsesContentUIDArtifactIdentity(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-unrelated-name", "node-a", "abc")
	pod := makeSourcePod()
	fc := &fakeCheckpointer{}
	w := makeNodeController(t, fc, content, pod)
	w.runtime = &fakeRuntime{resolveContainerPID: 7}

	require.NoError(t, w.reconcileSourcePod(context.Background(), pod))
	require.Eventually(t, fc.wasCalled, time.Second, 5*time.Millisecond)

	params := fc.lastParams()
	assert.Equal(t, string(content.UID), params.ContentUID)
	assert.Equal(t, filepath.Join(w.config.Storage.BasePath, "artifacts", string(content.UID), "containers", "main"), params.HostPath)

	// setSnapshotContentSucceeded runs after checkpointFn returns, so poll for the Ready condition rather than reading once.
	require.Eventually(t, func() bool {
		c := &snapshotv1alpha1.PodSnapshotContent{}
		if err := w.client.Get(context.Background(), types.NamespacedName{Name: content.Name}, c); err != nil {
			return false
		}
		return meta.FindStatusCondition(c.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady) != nil
	}, time.Second, 5*time.Millisecond)
}

func TestReconcileSnapshotContent_ResumeWritesReadyAndReleases(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	pod := makeSourcePod()
	fc := &fakeCheckpointer{}
	w := makeNodeController(t, fc, content, pod)
	w.runtime = &fakeRuntime{resolveContainerPID: 4242}
	released := false
	w.releaseCheckpointFn = func(containerPID int) error {
		assert.Equal(t, 4242, containerPID)
		released = true
		return nil
	}
	// Pre-create the artifact directory at the resolved destination so the resume check fires.
	dest := filepath.Join(w.config.Storage.BasePath, "artifacts", string(content.UID), "containers", "main")
	require.NoError(t, os.MkdirAll(dest, 0o755))
	require.NoError(t, snapshottypes.WriteManifest(dest, &snapshottypes.CheckpointManifest{Artifact: snapshottypes.ArtifactManifest{ContentUID: string(content.UID), ContainerName: "main"}}))

	require.NoError(t, w.reconcileSourcePod(context.Background(), pod))
	assert.False(t, fc.wasCalled())
	assert.True(t, released)
	got := getContent(t, w, content.Name)
	cond := meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady)
	require.NotNil(t, cond)
}

func TestReconcileSourcePod_ReadyDoesNotStarveNewDump(t *testing.T) {
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

	require.NoError(t, w.reconcileSourcePod(context.Background(), pod))
	require.Eventually(t, fc.wasCalled, time.Second, 5*time.Millisecond)
}

func TestReconcileSourcePod_InvalidReadySpecDoesNotStarveNewDump(t *testing.T) {
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

	require.NoError(t, w.reconcileSourcePod(context.Background(), pod))
	require.Eventually(t, fc.wasCalled, time.Second, 5*time.Millisecond)
}

func TestReconcileSourcePod_ReadyRetriesSentinel(t *testing.T) {
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
	w.runtime = &fakeRuntime{resolveContainerPID: 11}
	released := false
	w.releaseCheckpointFn = func(pid int) error {
		assert.Equal(t, 11, pid)
		released = true
		return nil
	}

	require.NoError(t, w.reconcileSourcePod(context.Background(), pod))
	assert.False(t, fc.wasCalled())
	assert.True(t, released)
}

func TestReconcileSourcePod_ReadyReleaseSkipsStalePodUID(t *testing.T) {
	ready := makeWorkOrder("podsnapshotcontent-ready", "node-a", "abc")
	meta.SetStatusCondition(&ready.Status.Conditions, metav1.Condition{
		Type:    snapshotv1alpha1.PodSnapshotConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  "Captured",
		Message: "Checkpoint captured and verified",
	})
	pod := makeSourcePod()
	pod.UID = types.UID("other-uid")
	fc := &fakeCheckpointer{}
	w := makeNodeController(t, fc, ready, pod)
	w.runtime = &fakeRuntime{resolveContainerPID: 11}
	released := false
	w.releaseCheckpointFn = func(int) error {
		released = true
		return nil
	}

	require.NoError(t, w.reconcileSourcePod(context.Background(), pod))
	assert.False(t, fc.wasCalled())
	assert.False(t, released)
}

func TestReconcileSourcePod_ReadyReleaseFailureKillsTarget(t *testing.T) {
	ready := makeWorkOrder("podsnapshotcontent-ready", "node-a", "abc")
	meta.SetStatusCondition(&ready.Status.Conditions, metav1.Condition{
		Type:    snapshotv1alpha1.PodSnapshotConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  "Captured",
		Message: "Checkpoint captured and verified",
	})
	pod := makeSourcePod()
	ctx, target := startKillableTarget(t)
	w := makeNodeController(t, &fakeCheckpointer{}, ready, pod)
	w.runtime = &fakeRuntime{resolveContainerPID: target.Process.Pid}
	w.releaseCheckpointFn = func(int) error { return errors.New("sentinel write rejected") }

	require.Error(t, w.reconcileSourcePod(context.Background(), pod))
	requireKilledBySIGKILL(t, ctx, target)
}

func TestRunCheckpoint_LeaseCancelledAfterDumpFailsAndKills(t *testing.T) {
	orig := checkpointLeaseRenewInterval
	checkpointLeaseRenewInterval = time.Millisecond
	t.Cleanup(func() { checkpointLeaseRenewInterval = orig })

	content := makeWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	w := makeNodeController(t, &fakeCheckpointer{}, content)
	released := false
	w.releaseCheckpointFn = func(int) error {
		released = true
		return nil
	}
	w.checkpointFn = func(ctx context.Context, params CheckpointParams) error {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
			t.Fatal("lease ctx was not cancelled")
			return nil
		}
	}
	ctx, target := startKillableTarget(t)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Namespace: "inference", UID: types.UID("pod-uid")}}
	leaseKey := client.ObjectKey{Namespace: "inference", Name: "checkpoint-lease-abc"}
	artifactPath := filepath.Join(w.config.Storage.BasePath, "abc", "versions", "1")

	w.runCheckpoint(context.Background(), content, pod, "main", "abc123", target.Process.Pid, "abc", artifactPath, leaseKey, "abc")

	assert.False(t, released)
	requireKilledBySIGKILL(t, ctx, target)
	got := getContent(t, w, content.Name)
	assert.Nil(t, meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady))
	failed := meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, "LeaseCancelled", failed.Reason)
}

func TestRunCheckpoint_LeaseCancelledConflictReadyDoesNotKill(t *testing.T) {
	orig := checkpointLeaseRenewInterval
	checkpointLeaseRenewInterval = time.Millisecond
	t.Cleanup(func() { checkpointLeaseRenewInterval = orig })

	stored := makeWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	meta.SetStatusCondition(&stored.Status.Conditions, metav1.Condition{
		Type:    snapshotv1alpha1.PodSnapshotConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  "Captured",
		Message: "Checkpoint captured and verified",
	})
	funcs := interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			sc, ok := obj.(*snapshotv1alpha1.PodSnapshotContent)
			if ok {
				if cond := meta.FindStatusCondition(sc.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed); cond != nil && cond.Status == metav1.ConditionTrue {
					return conflictErr()
				}
			}
			return c.Status().Patch(ctx, obj, patch, opts...)
		},
	}
	w := makeNodeControllerWithInterceptor(t, &fakeCheckpointer{}, funcs, stored)
	releaseCalls := 0
	w.releaseCheckpointFn = func(int) error {
		releaseCalls++
		return nil
	}
	w.checkpointFn = func(ctx context.Context, params CheckpointParams) error {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
			t.Fatal("lease ctx was not cancelled")
			return nil
		}
	}
	_, target := startKillableTarget(t)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Namespace: "inference", UID: types.UID("pod-uid")}}
	leaseKey := client.ObjectKey{Namespace: "inference", Name: "checkpoint-lease-abc"}
	artifactPath := filepath.Join(w.config.Storage.BasePath, "abc", "versions", "1")

	w.runCheckpoint(context.Background(), makeWorkOrder("podsnapshotcontent-abc", "node-a", "abc"), pod, "main", "abc123", target.Process.Pid, "abc", artifactPath, leaseKey, "abc")

	require.NoError(t, target.Process.Signal(syscall.Signal(0)), "stale holder must not SIGKILL after another holder marked Ready")
	require.Zero(t, releaseCalls, "stale holder must not release after Ready wins the conflict")
	require.NotNil(t, meta.FindStatusCondition(
		getContent(t, w, stored.Name).Status.Conditions,
		snapshotv1alpha1.PodSnapshotConditionReady,
	))
}

func TestReconcileSnapshotContent_PodNotFoundFails(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")
	w := makeNodeController(t, &fakeCheckpointer{}, content) // no pod

	w.reconcilePodSnapshotContent(context.Background(), content.Name)
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

	reason, _ := classifySourcePod(content, running("pod-uid", corev1.PodRunning, false))
	assert.Equal(t, "", reason)

	reason, _ = classifySourcePod(content, running("other-uid", corev1.PodRunning, false))
	assert.Equal(t, "StalePodReference", reason)

	for _, phase := range []corev1.PodPhase{corev1.PodFailed, corev1.PodSucceeded} {
		reason, _ = classifySourcePod(content, running("pod-uid", phase, false))
		assert.Equal(t, "SourcePodGone", reason)
	}

	reason, _ = classifySourcePod(content, running("pod-uid", corev1.PodRunning, true))
	assert.Equal(t, "SourcePodGone", reason)
}

func TestReconcileSnapshotContent_StalePodUIDFails(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Namespace: "inference", UID: types.UID("different-uid")},
		Spec:       corev1.PodSpec{NodeName: "node-a"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	w := makeNodeController(t, &fakeCheckpointer{}, content, pod)

	w.reconcilePodSnapshotContent(context.Background(), content.Name)
	got := getContent(t, w, content.Name)
	cond := meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
	require.NotNil(t, cond)
	assert.Equal(t, "StalePodReference", cond.Reason)
}

func TestReconcileSnapshotContent_PodFailedFails(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Namespace: "inference", UID: types.UID("pod-uid")},
		Spec:       corev1.PodSpec{NodeName: "node-a"},
		Status:     corev1.PodStatus{Phase: corev1.PodFailed},
	}
	w := makeNodeController(t, &fakeCheckpointer{}, content, pod)

	w.reconcilePodSnapshotContent(context.Background(), content.Name)
	got := getContent(t, w, content.Name)
	cond := meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
	require.NotNil(t, cond)
	assert.Equal(t, "SourcePodGone", cond.Reason)
}

func TestReconcileSnapshotContent_NotReadyQuiesceNoOp(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")
	pod := makeSourcePod()
	pod.Status.ContainerStatuses[0].Ready = false
	fc := &fakeCheckpointer{}
	w := makeNodeController(t, fc, content, pod)

	require.NoError(t, w.reconcileSourcePod(context.Background(), pod))
	assert.False(t, fc.wasCalled())
	got := getContent(t, w, content.Name)
	assert.Empty(t, got.Status.Conditions)
}

func TestReconcileSnapshotContent_CapturesFromPod(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	pod := makeSourcePod()
	fc := &fakeCheckpointer{}
	w := makeNodeController(t, fc, content, pod)
	w.runtime = &fakeRuntime{resolveContainerPID: 7}
	unblocked := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-unblocked:
		default:
			close(unblocked)
		}
	})
	w.checkpointFn = func(ctx context.Context, params CheckpointParams) error {
		err := fc.fn(ctx, params)
		<-unblocked
		return err
	}

	require.NoError(t, w.reconcileSourcePod(context.Background(), pod))

	// acquireLease runs synchronously before the goroutine starts. Hold the dump so
	// releaseLease cannot delete the Lease before this assertion.
	leaseName := checkpointLeaseName(string(content.UID), "main")
	_, err := w.clientset.CoordinationV1().Leases("inference").Get(context.Background(), leaseName, metav1.GetOptions{})
	require.NoError(t, err, "capture Lease must exist in namespace inference")
	close(unblocked)

	require.Eventually(t, fc.wasCalled, time.Second, 5*time.Millisecond)

	params := fc.lastParams()
	assert.Equal(t, string(content.UID), params.ContentUID)
	assert.Equal(t, "main", params.ContainerName)
	assert.Equal(t, "abc123", params.ContainerID)
	assert.Equal(t, 7, params.ContainerPID)
	dest := filepath.Join(w.config.Storage.BasePath, "artifacts", string(content.UID), "containers", "main")
	assert.Equal(t, dest, params.HostPath)

	// Ready is written after checkpointFn returns, so poll rather than reading once.
	require.Eventually(t, func() bool {
		c := &snapshotv1alpha1.PodSnapshotContent{}
		if err := w.client.Get(context.Background(), types.NamespacedName{Name: content.Name}, c); err != nil {
			return false
		}
		return meta.FindStatusCondition(c.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady) != nil
	}, time.Second, 5*time.Millisecond)
}

func TestRunCheckpoint_WritesReadyBeforeRelease(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	fc := &fakeCheckpointer{}
	w := makeNodeController(t, fc, content)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Namespace: "inference", UID: types.UID("pod-uid")}}
	leaseKey := client.ObjectKey{Namespace: "inference", Name: checkpointLeaseName(string(content.UID), "main")}
	artifactPath := filepath.Join(w.config.Storage.BasePath, "artifacts", string(content.UID), "containers", "main")
	released := false
	w.releaseCheckpointFn = func(containerPID int) error {
		assert.Equal(t, 7, containerPID)
		cond := meta.FindStatusCondition(
			getContent(t, w, content.Name).Status.Conditions,
			snapshotv1alpha1.PodSnapshotConditionReady,
		)
		require.NotNil(t, cond, "Ready must be observable before the target is released")
		released = true
		return nil
	}

	w.runCheckpoint(context.Background(), content, pod, "main", "abc123", 7, string(content.UID), artifactPath, leaseKey, string(content.UID)+"/main")

	assert.True(t, fc.wasCalled())
	assert.True(t, released)
}

func TestRunCheckpoint_WritesFailedOnError(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	fc := &fakeCheckpointer{err: errors.New("criu boom")}
	w := makeNodeController(t, fc, content)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Namespace: "inference", UID: types.UID("pod-uid")}}
	leaseKey := client.ObjectKey{Namespace: "inference", Name: checkpointLeaseName(string(content.UID), "main")}
	artifactPath := filepath.Join(w.config.Storage.BasePath, "artifacts", string(content.UID), "containers", "main")

	w.runCheckpoint(context.Background(), content, pod, "main", "abc123", 7, string(content.UID), artifactPath, leaseKey, string(content.UID)+"/main")

	got := getContent(t, w, content.Name)
	cond := meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
	require.NotNil(t, cond)
	assert.Equal(t, "CheckpointFailed", cond.Reason)
}

func TestRunCheckpoint_ReleaseFailureKillsTarget(t *testing.T) {
	ctx, target := startKillableTarget(t)

	content := makeWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	w := makeNodeController(t, &fakeCheckpointer{}, content)
	w.releaseCheckpointFn = func(int) error { return errors.New("sentinel write rejected") }
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Namespace: "inference", UID: types.UID("pod-uid")}}
	leaseKey := client.ObjectKey{Namespace: "inference", Name: checkpointLeaseName(string(content.UID), "main")}
	artifactPath := filepath.Join(w.config.Storage.BasePath, "artifacts", string(content.UID), "containers", "main")

	w.runCheckpoint(context.Background(), content, pod, "main", "abc123", target.Process.Pid, string(content.UID), artifactPath, leaseKey, string(content.UID)+"/main")

	requireKilledBySIGKILL(t, ctx, target)
	require.NotNil(t, meta.FindStatusCondition(
		getContent(t, w, content.Name).Status.Conditions,
		snapshotv1alpha1.PodSnapshotConditionReady,
	))
}

// startKillableTarget starts a short-lived sleep process the test can assert was SIGKILLed.
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

func requireKilledBySIGKILL(t *testing.T, ctx context.Context, target *exec.Cmd) {
	t.Helper()
	err := target.Wait()
	require.NoError(t, ctx.Err(), "killCheckpointProcess did not terminate the target before the test deadline")
	require.Error(t, err)
	waitStatus, ok := target.ProcessState.Sys().(syscall.WaitStatus)
	require.True(t, ok)
	assert.Equal(t, syscall.SIGKILL, waitStatus.Signal())
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

func TestReconcileSourcePod_TriggersUnstick(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	content.CreationTimestamp = metav1.Unix(1000, 0)
	pod := podWithFailedSibling()
	fc := &fakeCheckpointer{}
	rt := &fakeRuntime{}
	w := makeNodeController(t, fc, content, pod)
	w.runtime = rt

	require.NoError(t, w.reconcileSourcePod(context.Background(), pod))

	got := getContent(t, w, content.Name)
	cond := meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
	require.NotNil(t, cond)
	assert.Equal(t, "CheckpointContainerFailed", cond.Reason)
	assert.Equal(t, []string{"main-id"}, rt.resolvedContainerIDs)
	assert.False(t, fc.wasCalled())
}

func TestReconcileSourcePod_PodNotIndexedNoOp(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	pod := podWithFailedSibling()
	w := makeNodeController(t, &fakeCheckpointer{}, content, pod)
	w.contentIndexer = seedIndex(t) // override: empty index

	require.NoError(t, w.reconcileSourcePod(context.Background(), pod))
	assert.Empty(t, getContent(t, w, content.Name).Status.Conditions)
}

func TestReconcileSourcePod_IndexErrorReturned(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	pod := podWithFailedSibling()
	w := makeNodeController(t, &fakeCheckpointer{}, content, pod)
	// Indexer without podRefIndex registered → ByIndex returns an error; reconcile surfaces it
	// (the informer handler logs it) and writes no status.
	w.contentIndexer = cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})

	require.Error(t, w.reconcileSourcePod(context.Background(), pod))
	assert.Empty(t, getContent(t, w, content.Name).Status.Conditions)
}

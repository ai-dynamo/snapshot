// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/testr"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

const testNodeName = "test-node"
const testContainerID = "test-container"

// fakeRuntime is a minimal Runtime implementation for controller reconciliation
// tests.
type fakeRuntime struct {
	containerIDByPod     string
	resolvedContainerIDs []string
	// resolveContainerPID, when set, is returned by ResolveContainer with no error so the
	// capture path can advance past container resolution.
	resolveContainerPID int
}

var _ snapshotruntime.Runtime = (*fakeRuntime)(nil)

func (r *fakeRuntime) ResolveContainer(ctx context.Context, id string) (int, *specs.Spec, error) {
	r.resolvedContainerIDs = append(r.resolvedContainerIDs, id)
	if r.resolveContainerPID > 0 {
		return r.resolveContainerPID, nil, nil
	}
	return 0, nil, errors.New("not implemented")
}
func (r *fakeRuntime) ResolveContainerIDByPod(ctx context.Context, pod, ns, ctr string) (string, error) {
	if r.containerIDByPod != "" {
		return r.containerIDByPod, nil
	}
	return "", errors.New("not implemented")
}
func (r *fakeRuntime) ResolveContainerByPod(ctx context.Context, pod, ns, ctr string) (int, *specs.Spec, error) {
	return 0, nil, errors.New("not implemented")
}
func (r *fakeRuntime) Close() error { return nil }

// noopInjector is a no-op RestoreMounter used in tests that do not exercise
// the injection path. It prevents a nil-pointer panic if runRestore is ever
// reached by a test that was previously relying on Phase 1 failing first.
type noopInjector struct{}

func (noopInjector) MountBundle(_ context.Context, _ int) (nsmount.MountPoint, error) {
	return noopMountPoint{}, nil
}

func (noopInjector) MountArtifact(_ context.Context, _ nsmount.MountPoint, _ string) (nsmount.MountPoint, error) {
	return noopMountPoint{}, nil
}

func (noopInjector) MountPageBroker(_ context.Context, _ nsmount.MountPoint, _ string) (nsmount.MountPoint, error) {
	return noopMountPoint{}, nil
}

type noopMountPoint struct{}

func (noopMountPoint) Unmount(context.Context) error { return nil }
func (noopMountPoint) NsFd() *os.File                { return nil }

// errorInjector always returns the wrapped error from either mount role.
type errorInjector struct{ err error }

func (e errorInjector) MountBundle(_ context.Context, _ int) (nsmount.MountPoint, error) {
	return nil, e.err
}

func (e errorInjector) MountArtifact(_ context.Context, _ nsmount.MountPoint, _ string) (nsmount.MountPoint, error) {
	return nil, e.err
}

func (e errorInjector) MountPageBroker(_ context.Context, _ nsmount.MountPoint, _ string) (nsmount.MountPoint, error) {
	return nil, e.err
}

type recordedMountCall struct {
	src            string
	dst            string
	namespaceMount nsmount.MountPoint
}

type recordingInjector struct {
	calls       *[]recordedMountCall
	bundleMount nsmount.MountPoint
	bundleErr   error
	artifactErr error
}

func (r recordingInjector) MountBundle(_ context.Context, _ int) (nsmount.MountPoint, error) {
	*r.calls = append(*r.calls, recordedMountCall{src: nsmount.SnapshotBinSrc, dst: nsmount.SnapshotBinDst})
	if r.bundleErr != nil {
		return nil, r.bundleErr
	}
	return r.bundleMount, nil
}

func (r recordingInjector) MountArtifact(_ context.Context, namespaceMount nsmount.MountPoint, src string) (nsmount.MountPoint, error) {
	*r.calls = append(*r.calls, recordedMountCall{src: src, dst: nsmount.CheckpointDst, namespaceMount: namespaceMount})
	if r.artifactErr != nil {
		return nil, r.artifactErr
	}
	return noopMountPoint{}, nil
}

func (r recordingInjector) MountPageBroker(_ context.Context, namespaceMount nsmount.MountPoint, src string) (nsmount.MountPoint, error) {
	*r.calls = append(*r.calls, recordedMountCall{src: src, dst: nsmount.PageBrokerDst, namespaceMount: namespaceMount})
	if r.artifactErr != nil {
		return nil, r.artifactErr
	}
	return noopMountPoint{}, nil
}

var _ executor.RestoreMounter = noopInjector{}
var _ executor.RestoreMounter = errorInjector{}
var _ executor.RestoreMounter = recordingInjector{}

func TestNewDefaultControllerSetsDefaultOperations(t *testing.T) {
	w := newDefaultController(
		&types.AgentConfig{},
		fake.NewClientset(),
		nil,
		nil,
		&fakeRuntime{},
		noopInjector{},
		testr.New(t),
	)
	if w.checkpointFn == nil || w.restoreFn == nil || w.writeControlSentinelFn == nil {
		t.Fatal("default controller operations must be initialized")
	}
}

// makeTestController creates a NodeController with a fake k8s client and nil executors.
// The fake clientset is empty so any goroutine launched by the restore path will fail on
// the first annotatePod call and exit cleanly.
func makeTestController(t *testing.T, objs ...runtime.Object) *NodeController {
	t.Helper()
	return &NodeController{
		config: &types.AgentConfig{
			NodeName: testNodeName,
			Storage: types.StorageSpec{
				Type:     "pvc",
				BasePath: t.TempDir(),
			},
		},
		clientset:              fake.NewClientset(objs...),
		runtime:                &fakeRuntime{},
		injector:               noopInjector{},
		restoreFn:              executor.Restore,
		writeControlSentinelFn: func(int, string) error { return nil },
		log:                    testr.New(t),
		holderID:               "test-holder",
		inFlight:               make(map[string]struct{}),
		stopCh:                 make(chan struct{}),
	}
}

func sawEventReason(clientset *fake.Clientset, reason string) bool {
	for _, action := range clientset.Actions() {
		create, ok := action.(clientgotesting.CreateAction)
		if !ok || create.GetResource().Resource != "events" {
			continue
		}
		event, ok := create.GetObject().(*corev1.Event)
		if ok && event.Reason == reason {
			return true
		}
	}
	return false
}

func observeEventReason(clientset *fake.Clientset, reason string) <-chan struct{} {
	seen := make(chan struct{}, 1)
	clientset.PrependReactor("create", "events", func(action clientgotesting.Action) (bool, runtime.Object, error) {
		create, ok := action.(clientgotesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		event, ok := create.GetObject().(*corev1.Event)
		if ok && event.Reason == reason {
			select {
			case seen <- struct{}{}:
			default:
			}
		}
		return false, nil, nil
	})
	return seen
}

func waitForSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func makePod(name, namespace, nodeName string, phase corev1.PodPhase, ready bool, labels, annotations map[string]string) *corev1.Pod {
	var conditions []corev1.PodCondition
	if ready {
		conditions = append(conditions, corev1.PodCondition{
			Type:   corev1.PodReady,
			Status: corev1.ConditionTrue,
		})
	}
	// The snapshot contract requires the target-containers annotation on
	// every checkpoint/restore pod; stamp it here so individual cases do
	// not have to repeat themselves.
	merged := map[string]string{
		snapshotv1alpha1.TargetContainersAnnotation: "main",
	}
	for k, v := range annotations {
		merged[k] = v
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      labels,
			Annotations: merged,
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{
				{Name: "main"},
			},
		},
		Status: corev1.PodStatus{
			Phase:      phase,
			Conditions: conditions,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", Ready: ready, ContainerID: "containerd://" + testContainerID},
			},
		},
	}
}

func TestArtifactPathForPod(t *testing.T) {
	pod := makePod(
		"test-pod",
		"default",
		testNodeName,
		corev1.PodRunning,
		true,
		nil,
		map[string]string{
			snapshotv1alpha1.CheckpointArtifactVersionAnnotation: "2",
		},
	)

	t.Run("uses the agent base path and pod version", func(t *testing.T) {
		w := makeTestController(t)
		w.config.Storage.BasePath = "/checkpoints"

		got, err := w.artifactPathForPod(pod, "abc123")
		if err != nil {
			t.Fatalf("artifactPathForPod() error = %v", err)
		}
		expected := "/checkpoints/abc123/versions/2"
		if got != expected {
			t.Fatalf("artifact path = %q, want %q", got, expected)
		}
	})

	t.Run("defaults the artifact version", func(t *testing.T) {
		unversioned := pod.DeepCopy()
		delete(unversioned.Annotations, snapshotv1alpha1.CheckpointArtifactVersionAnnotation)
		w := makeTestController(t)
		w.config.Storage.BasePath = "/checkpoints"
		got, err := w.artifactPathForPod(unversioned, "abc123")
		if err != nil {
			t.Fatalf("artifactPathForPod() error = %v", err)
		}
		expected := "/checkpoints/abc123/versions/" + snapshotv1alpha1.DefaultCheckpointArtifactVersion
		if got != expected {
			t.Fatalf("artifact path = %q, want %q", got, expected)
		}
	})

	t.Run("ignores legacy pod storage base path", func(t *testing.T) {
		annotatedPod := pod.DeepCopy()
		annotatedPod.Annotations["nvidia.com/snapshot-storage-base-path"] = "/pod-checkpoints/"
		w := makeTestController(t)
		w.config.Storage.BasePath = "/agent-checkpoints"
		got, err := w.artifactPathForPod(annotatedPod, "abc123")
		if err != nil {
			t.Fatalf("artifactPathForPod() error = %v", err)
		}
		expected := "/agent-checkpoints/abc123/versions/2"
		if got != expected {
			t.Fatalf("artifact path = %q, want %q", got, expected)
		}
	})

	t.Run("rejects unsafe components", func(t *testing.T) {
		w := makeTestController(t)
		w.config.Storage.BasePath = "/checkpoints"
		if _, err := w.artifactPathForPod(pod, "../escape"); err == nil {
			t.Fatal("expected unsafe checkpoint ID to fail")
		}
		unsafeVersion := pod.DeepCopy()
		unsafeVersion.Annotations[snapshotv1alpha1.CheckpointArtifactVersionAnnotation] = "../escape"
		if _, err := w.artifactPathForPod(unsafeVersion, "abc123"); err == nil {
			t.Fatal("expected unsafe artifact version to fail")
		}
	})
}

func TestTweakNodePodListOptions(t *testing.T) {
	opts := &metav1.ListOptions{}
	tweakNodePodListOptions("snapshot.nvidia.com/checkpoint-id", testNodeName)(opts)

	if opts.LabelSelector != "snapshot.nvidia.com/checkpoint-id" {
		t.Fatalf("label selector = %q", opts.LabelSelector)
	}
	if opts.FieldSelector != "spec.nodeName="+testNodeName {
		t.Fatalf("field selector = %q", opts.FieldSelector)
	}
}

func TestRestoreCheckpointReady(t *testing.T) {
	w := makeTestController(t)
	log := testr.New(t)

	t.Run("existing directory is ready", func(t *testing.T) {
		dir := t.TempDir()
		ready, err := w.restoreCheckpointReady(log, "default/test-pod", "abc123", dir)
		if err != nil {
			t.Fatalf("restoreCheckpointReady() error = %v", err)
		}
		if !ready {
			t.Fatal("expected checkpoint directory to be ready")
		}
	})

	t.Run("missing directory is not ready", func(t *testing.T) {
		ready, err := w.restoreCheckpointReady(log, "default/test-pod", "abc123", filepath.Join(t.TempDir(), "missing"))
		if err != nil {
			t.Fatalf("restoreCheckpointReady() error = %v", err)
		}
		if ready {
			t.Fatal("expected missing checkpoint directory to be not ready")
		}
	})

	t.Run("file is rejected", func(t *testing.T) {
		filePath := filepath.Join(t.TempDir(), "checkpoint")
		if err := os.WriteFile(filePath, []byte("not a directory"), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}

		_, err := w.restoreCheckpointReady(log, "default/test-pod", "abc123", filePath)
		if err == nil {
			t.Fatal("expected file checkpoint location to be rejected")
		}
		if !strings.Contains(err.Error(), "not a directory") {
			t.Fatalf("expected not-a-directory error, got: %v", err)
		}
	})
}

func TestReconcileRestorePod(t *testing.T) {
	tests := []struct {
		name                  string
		nodeName              string
		phase                 corev1.PodPhase
		ready                 bool
		hash                  string
		annotationStatus      string
		annotationContainerID string
		createDir             bool // whether to create the checkpoint dir on disk
		preSeed               bool
		want                  bool
	}{
		{
			name:      "happy path",
			nodeName:  testNodeName,
			phase:     corev1.PodRunning,
			ready:     false,
			hash:      "abc123",
			createDir: true,
			want:      true,
		},
		{
			name:      "wrong node",
			nodeName:  "other-node",
			phase:     corev1.PodRunning,
			ready:     false,
			hash:      "abc123",
			createDir: true,
			want:      false,
		},
		{
			name:      "pending pod with status container id still restores",
			nodeName:  testNodeName,
			phase:     corev1.PodPending,
			ready:     false,
			hash:      "abc123",
			createDir: true,
			want:      true,
		},
		{
			name:      "succeeded pod does not restore",
			nodeName:  testNodeName,
			phase:     corev1.PodSucceeded,
			ready:     false,
			hash:      "abc123",
			createDir: true,
			want:      false,
		},
		{
			name:      "failed pod does not restore",
			nodeName:  testNodeName,
			phase:     corev1.PodFailed,
			ready:     false,
			hash:      "abc123",
			createDir: true,
			want:      false,
		},
		{
			name:      "unknown pod does not restore",
			nodeName:  testNodeName,
			phase:     corev1.PodUnknown,
			ready:     false,
			hash:      "abc123",
			createDir: true,
			want:      false,
		},
		{
			name:      "ready placeholder still restores",
			nodeName:  testNodeName,
			phase:     corev1.PodRunning,
			ready:     true,
			hash:      "abc123",
			createDir: true,
			want:      true,
		},
		{
			name:     "missing hash",
			nodeName: testNodeName,
			phase:    corev1.PodRunning,
			ready:    false,
			hash:     "",
			want:     false,
		},
		{
			name:      "invalid hash with path traversal",
			nodeName:  testNodeName,
			phase:     corev1.PodRunning,
			ready:     false,
			hash:      "../bad",
			createDir: true,
			want:      false,
		},
		{
			name:                  "already completed for same container",
			nodeName:              testNodeName,
			phase:                 corev1.PodRunning,
			ready:                 false,
			hash:                  "abc123",
			annotationStatus:      "completed",
			annotationContainerID: testContainerID,
			createDir:             true,
			want:                  false,
		},
		{
			name:                  "in progress for same container retries after restart",
			nodeName:              testNodeName,
			phase:                 corev1.PodRunning,
			ready:                 false,
			hash:                  "abc123",
			annotationStatus:      "in_progress",
			annotationContainerID: testContainerID,
			createDir:             true,
			want:                  true,
		},
		{
			name:                  "already failed for same container",
			nodeName:              testNodeName,
			phase:                 corev1.PodRunning,
			ready:                 false,
			hash:                  "abc123",
			annotationStatus:      "failed",
			annotationContainerID: testContainerID,
			createDir:             true,
			want:                  false,
		},
		{
			name:                  "completed for previous container retries",
			nodeName:              testNodeName,
			phase:                 corev1.PodRunning,
			ready:                 false,
			hash:                  "abc123",
			annotationStatus:      "completed",
			annotationContainerID: "old-container",
			createDir:             true,
			want:                  true,
		},
		{
			name:                  "failed for previous container retries",
			nodeName:              testNodeName,
			phase:                 corev1.PodRunning,
			ready:                 false,
			hash:                  "abc123",
			annotationStatus:      "failed",
			annotationContainerID: "old-container",
			createDir:             true,
			want:                  true,
		},
		{
			name:                  "in progress for previous container retries",
			nodeName:              testNodeName,
			phase:                 corev1.PodRunning,
			ready:                 false,
			hash:                  "abc123",
			annotationStatus:      "in_progress",
			annotationContainerID: "old-container",
			createDir:             true,
			want:                  true,
		},
		{
			name:      "checkpoint not on disk",
			nodeName:  testNodeName,
			phase:     corev1.PodRunning,
			ready:     false,
			hash:      "abc123",
			createDir: false,
			want:      false,
		},
		{
			name:      "duplicate in-flight",
			nodeName:  testNodeName,
			phase:     corev1.PodRunning,
			ready:     false,
			hash:      "abc123",
			createDir: true,
			preSeed:   true,
			want:      false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Restore pods are identified by snapshot-agent as
			// (CheckpointIDLabel present, CheckpointSourceLabel absent),
			// so the restore informer's label selector does the filtering.
			// The hash-missing case deliberately omits the label to exercise
			// the early-return branch in reconcileRestorePod.
			labels := map[string]string{}
			if tc.hash != "" {
				labels[snapshotv1alpha1.CheckpointIDLabel] = tc.hash
			}

			var annotations map[string]string
			if tc.annotationStatus != "" {
				annotations = map[string]string{
					snapshotv1alpha1.RestoreStatusAnnotationPrefix + "main":      tc.annotationStatus,
					snapshotv1alpha1.RestoreContainerIDAnnotationPrefix + "main": tc.annotationContainerID,
				}
			}

			pod := makePod("test-pod", "default", tc.nodeName, tc.phase, tc.ready, labels, annotations)
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name:        "main",
				Ready:       tc.ready,
				ContainerID: "containerd://" + testContainerID,
			}}
			w := makeTestController(t, pod)
			w.restoreFn = func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (int, error) {
				return 0, errors.New("test restore stopped")
			}
			workerFinished := observeEventReason(w.clientset.(*fake.Clientset), "RestoreWorkerFailed")

			if tc.createDir && tc.hash != "" {
				dir := filepath.Join(w.config.Storage.BasePath, tc.hash, "versions", snapshotv1alpha1.DefaultCheckpointArtifactVersion)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatalf("failed to create checkpoint dir: %v", err)
				}
			}

			ctx := context.Background()

			if tc.preSeed {
				w.inFlight["default/test-pod/main/"+testContainerID] = struct{}{}
			}

			w.reconcileRestorePod(ctx, pod)

			triggered := sawEventReason(w.clientset.(*fake.Clientset), "RestoreRequested")

			if triggered != tc.want {
				t.Errorf("triggered = %v, want %v (inFlight=%d, preSeed=%v, actions=%#v)", triggered, tc.want, len(w.inFlight), tc.preSeed, w.clientset.(*fake.Clientset).Actions())
			}

			if tc.want {
				waitForSignal(t, workerFinished, "restore worker completion")
			}
		})
	}
}

func TestReconcileRestorePodRejectsTargetNameThatCannotFitStatusAnnotation(t *testing.T) {
	checkpointID := "abc123"
	containerName := "restore-target-with-long-name-123456"
	w := makeTestController(t)
	dir := filepath.Join(w.config.Storage.BasePath, checkpointID, "versions", snapshotv1alpha1.DefaultCheckpointArtifactVersion)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("failed to create checkpoint dir: %v", err)
	}

	pod := makePod(
		"test-pod",
		"default",
		testNodeName,
		corev1.PodRunning,
		false,
		map[string]string{snapshotv1alpha1.CheckpointIDLabel: checkpointID},
		map[string]string{snapshotv1alpha1.TargetContainersAnnotation: containerName},
	)
	pod.Spec.Containers[0].Name = containerName
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:        containerName,
		ContainerID: "containerd://" + testContainerID,
	}}

	w.reconcileRestorePod(context.Background(), pod)
	if len(w.inFlight) != 0 {
		t.Fatalf("expected restore not to start for overlong annotation key, got inFlight=%v", w.inFlight)
	}
}

func TestReconcileRestorePodResolvesContainerBeforePodStatus(t *testing.T) {
	labels := map[string]string{
		snapshotv1alpha1.CheckpointIDLabel: "abc123",
	}

	pod := makePod("test-pod", "default", testNodeName, corev1.PodRunning, false, labels, nil)
	pod.Status.ContainerStatuses = nil
	w := makeTestController(t, pod)
	w.runtime = &fakeRuntime{containerIDByPod: testContainerID}
	w.restoreFn = func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (int, error) {
		return 0, errors.New("test restore stopped")
	}
	clientset := w.clientset.(*fake.Clientset)
	restoreRequested := observeEventReason(clientset, "RestoreRequested")
	workerFinished := observeEventReason(clientset, "RestoreWorkerFailed")
	dir := filepath.Join(w.config.Storage.BasePath, "abc123", "versions", snapshotv1alpha1.DefaultCheckpointArtifactVersion)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("failed to create checkpoint dir: %v", err)
	}

	w.reconcileRestorePod(context.Background(), pod)
	waitForSignal(t, restoreRequested, "RestoreRequested event after node-runtime container resolution")
	waitForSignal(t, workerFinished, "restore worker completion")
}

func TestReconcileRestorePodPollsRuntimeBeforePodRunning(t *testing.T) {
	labels := map[string]string{
		snapshotv1alpha1.CheckpointIDLabel: "abc123",
	}

	pod := makePod("test-pod", "default", testNodeName, corev1.PodPending, false, labels, nil)
	pod.Status.ContainerStatuses = nil
	w := makeTestController(t, pod)
	w.runtime = &fakeRuntime{containerIDByPod: testContainerID}
	w.restoreFn = func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (int, error) {
		return 0, errors.New("test restore stopped")
	}
	clientset := w.clientset.(*fake.Clientset)
	restoreRequested := observeEventReason(clientset, "RestoreRequested")
	workerFinished := observeEventReason(clientset, "RestoreWorkerFailed")
	dir := filepath.Join(w.config.Storage.BasePath, "abc123", "versions", snapshotv1alpha1.DefaultCheckpointArtifactVersion)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("failed to create checkpoint dir: %v", err)
	}

	w.reconcileRestorePod(context.Background(), pod)
	waitForSignal(t, restoreRequested, "RestoreRequested event from runtime polling before PodRunning")
	waitForSignal(t, workerFinished, "restore worker completion")
}

func TestPollForContainerIDSkipsTerminalLivePod(t *testing.T) {
	checkpointID := "abc123"
	labels := map[string]string{
		snapshotv1alpha1.CheckpointIDLabel: checkpointID,
	}
	stalePod := makePod("test-pod", "default", testNodeName, corev1.PodPending, false, labels, nil)
	stalePod.Status.ContainerStatuses = nil
	livePod := stalePod.DeepCopy()
	livePod.Status.Phase = corev1.PodSucceeded

	w := makeTestController(t, livePod)
	w.runtime = &fakeRuntime{containerIDByPod: testContainerID}
	clientset := w.clientset.(*fake.Clientset)
	dir := filepath.Join(w.config.Storage.BasePath, checkpointID, "versions", snapshotv1alpha1.DefaultCheckpointArtifactVersion)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("failed to create checkpoint dir: %v", err)
	}

	resolveKey := "default/test-pod/main/resolve"
	w.inFlight[resolveKey] = struct{}{}
	w.pollForContainerID(context.Background(), stalePod, "main", checkpointID, "default/test-pod", resolveKey)

	if _, held := w.inFlight[resolveKey]; held {
		t.Fatal("expected resolver key to be released")
	}
	for _, action := range clientset.Actions() {
		create, ok := action.(clientgotesting.CreateAction)
		if !ok || create.GetResource().Resource != "events" {
			continue
		}
		event, ok := create.GetObject().(*corev1.Event)
		if ok && event.Reason == "RestoreRequested" {
			t.Fatalf("stale resolver should not start restore for terminal live pod; actions=%#v", clientset.Actions())
		}
	}
}

func TestPollForContainerIDSkipsWhenRestoreAttemptAlreadyHeld(t *testing.T) {
	checkpointID := "abc123"
	labels := map[string]string{
		snapshotv1alpha1.CheckpointIDLabel: checkpointID,
	}
	stalePod := makePod("test-pod", "default", testNodeName, corev1.PodRunning, false, labels, nil)
	stalePod.Status.ContainerStatuses = nil

	w := makeTestController(t, stalePod)
	w.runtime = &fakeRuntime{containerIDByPod: testContainerID}
	clientset := w.clientset.(*fake.Clientset)
	dir := filepath.Join(w.config.Storage.BasePath, checkpointID, "versions", snapshotv1alpha1.DefaultCheckpointArtifactVersion)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("failed to create checkpoint dir: %v", err)
	}

	resolveKey := "default/test-pod/main/resolve"
	restoreAttemptKey := "default/test-pod/main/" + testContainerID
	w.inFlight[resolveKey] = struct{}{}
	w.inFlight[restoreAttemptKey] = struct{}{}
	w.pollForContainerID(context.Background(), stalePod, "main", checkpointID, "default/test-pod", resolveKey)

	if _, held := w.inFlight[resolveKey]; held {
		t.Fatal("expected resolver key to be released")
	}
	if _, held := w.inFlight[restoreAttemptKey]; !held {
		t.Fatal("expected existing restore attempt key to remain held")
	}
	for _, action := range clientset.Actions() {
		create, ok := action.(clientgotesting.CreateAction)
		if !ok || create.GetResource().Resource != "events" {
			continue
		}
		event, ok := create.GetObject().(*corev1.Event)
		if ok && event.Reason == "RestoreRequested" {
			t.Fatalf("stale resolver should not start restore while attempt key is held; actions=%#v", clientset.Actions())
		}
	}
}

func TestRunRestoreFailureEvents(t *testing.T) {
	checkpointID := "test-checkpoint"
	pod := makePod("test-pod", "default", testNodeName, corev1.PodRunning, true,
		map[string]string{snapshotv1alpha1.CheckpointIDLabel: checkpointID}, nil)

	// Write a minimal manifest so inspectRestore can load it.
	basePath := t.TempDir()
	checkpointDir := filepath.Join(basePath, checkpointID, "versions", snapshotv1alpha1.DefaultCheckpointArtifactVersion)
	if err := os.MkdirAll(checkpointDir, 0o755); err != nil {
		t.Fatalf("create checkpoint dir: %v", err)
	}
	if err := types.WriteManifest(checkpointDir, &types.CheckpointManifest{
		CheckpointID: checkpointID,
		K8s:          types.SourcePodManifest{PodNamespace: "default"},
	}); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	runRestore := func(t *testing.T, mounter executor.RestoreMounter) *NodeController {
		t.Helper()
		w := makeTestController(t, pod)
		w.config.Storage.BasePath = basePath
		// math.MaxInt32 is above any real kernel pid_max (≤4194304) so SendSignalToPID
		// returns ESRCH instead of killing the test process.
		w.runtime = &fakeRuntime{resolveContainerPID: math.MaxInt32}
		w.injector = mounter
		_ = w.runRestore(
			context.Background(), pod, "main", "ctr-abc", checkpointID,
			"default/test-pod/main/ctr-abc",
			time.Time{},
		)
		return w
	}

	injectErr := errors.New("injector unavailable")
	t.Run("bundle mount failure", func(t *testing.T) {
		w := runRestore(t, errorInjector{err: injectErr})
		if !sawEventReason(w.clientset.(*fake.Clientset), "RestoreFailed") {
			t.Fatal("expected RestoreFailed event when bundle mount fails")
		}
	})

	t.Run("artifact mount failure and role paths", func(t *testing.T) {
		var calls []recordedMountCall
		bundleMount := &noopMountPoint{}
		w := runRestore(t, recordingInjector{calls: &calls, bundleMount: bundleMount, artifactErr: injectErr})
		if !sawEventReason(w.clientset.(*fake.Clientset), "RestoreFailed") {
			t.Fatal("expected RestoreFailed event when artifact mount fails")
		}
		want := []recordedMountCall{
			{src: nsmount.SnapshotBinSrc, dst: nsmount.SnapshotBinDst},
			{src: checkpointDir, dst: nsmount.CheckpointDst},
		}
		if len(calls) != len(want) {
			t.Fatalf("mount calls = %#v, want %#v", calls, want)
		}
		for i := range want {
			if calls[i].src != want[i].src || calls[i].dst != want[i].dst {
				t.Errorf("mount call[%d] = %#v, want %#v", i, calls[i], want[i])
			}
		}
		if calls[1].namespaceMount != bundleMount {
			t.Fatalf("artifact namespace mount = %T %p, want bundle mount %p", calls[1].namespaceMount, calls[1].namespaceMount, bundleMount)
		}
	})

	t.Run("cleanup failure is log-only and restore completes", func(t *testing.T) {
		w := makeTestController(t, pod)
		w.config.Storage.BasePath = basePath
		rt := &fakeRuntime{}
		w.runtime = rt
		w.restoreFn = func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (int, error) {
			return 4242, executor.NewRestoreCleanupError(fmt.Errorf(
				"unmount checkpoint artifact from placeholder: %w",
				errors.New("unmount failed"),
			))
		}

		err := w.runRestore(
			context.Background(), pod, "main", "ctr-abc", checkpointID,
			"default/test-pod/main/ctr-abc",
			time.Time{},
		)
		if err != nil {
			t.Fatalf("cleanup failure should not fail restore: %v", err)
		}
		if len(rt.resolvedContainerIDs) != 0 {
			t.Fatalf("cleanup failure reached kill path: resolved container IDs = %v", rt.resolvedContainerIDs)
		}
		if !sawEventReason(w.clientset.(*fake.Clientset), "RestoreSucceeded") {
			t.Fatal("expected RestoreSucceeded event after cleanup failure")
		}
		if !sawEventReason(w.clientset.(*fake.Clientset), "RestoreCleanupFailed") {
			t.Fatal("expected RestoreCleanupFailed warning event")
		}
		if sawEventReason(w.clientset.(*fake.Clientset), restoreFailedReason) {
			t.Fatal("unexpected RestoreFailed event after cleanup failure")
		}
		updated, err := w.clientset.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get restore pod: %v", err)
		}
		keys, err := snapshotv1alpha1.RestoreStatusAnnotationKeysFor("main")
		if err != nil {
			t.Fatal(err)
		}
		if got := updated.Annotations[keys.Status]; got != snapshotv1alpha1.RestoreStatusCompleted {
			t.Fatalf("restore status = %q, want %q", got, snapshotv1alpha1.RestoreStatusCompleted)
		}
	})
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/testr"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

const (
	testNodeName    = "test-node"
	testContainerID = "test-container"
)

type fakeRuntime struct {
	containerIDByPod     string
	resolvedContainerIDs []string
	resolveContainerPID  int
}

var _ snapshotruntime.Runtime = (*fakeRuntime)(nil)

func (r *fakeRuntime) ResolveContainer(_ context.Context, id string) (int, *specs.Spec, error) {
	r.resolvedContainerIDs = append(r.resolvedContainerIDs, id)
	if r.resolveContainerPID > 0 {
		return r.resolveContainerPID, &specs.Spec{}, nil
	}
	return 0, nil, errors.New("not implemented")
}

func (r *fakeRuntime) ResolveContainerIDByPod(_ context.Context, _, _, _ string) (string, error) {
	if r.containerIDByPod != "" {
		return r.containerIDByPod, nil
	}
	return "", errors.New("not implemented")
}

func (r *fakeRuntime) ResolveContainerByPod(_ context.Context, _, _, _ string) (int, *specs.Spec, error) {
	return 0, nil, errors.New("not implemented")
}

func (r *fakeRuntime) Close() error { return nil }

type noopInjector struct{}

func (noopInjector) MountBundle(_ context.Context, _ int) (nsmount.MountPoint, error) {
	return noopMountPoint{}, nil
}

func (noopInjector) MountArtifact(_ context.Context, _ nsmount.MountPoint, _ string) (nsmount.MountPoint, error) {
	return noopMountPoint{}, nil
}

type noopMountPoint struct{}

func (noopMountPoint) Unmount(context.Context) error { return nil }
func (noopMountPoint) NsFd() *os.File                { return nil }

var _ executor.RestoreMounter = noopInjector{}

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
	if w.checkpointFn == nil || w.restoreFn == nil || w.writeControlSentinelFn == nil || w.releaseCheckpointFn == nil {
		t.Fatal("default controller operations must be initialized")
	}
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, snapshotv1alpha1.AddToScheme(scheme))
	return scheme
}

func makeTestController(t *testing.T, pod *corev1.Pod, apiObjects ...runtime.Object) *NodeController {
	t.Helper()
	clientObjects := make([]runtime.Object, len(apiObjects))
	copy(clientObjects, apiObjects)
	clientBuilder := ctrlfake.NewClientBuilder().WithScheme(testScheme(t))
	for _, object := range clientObjects {
		clientBuilder = clientBuilder.WithRuntimeObjects(object)
	}
	coreObjects := []runtime.Object{}
	if pod != nil {
		coreObjects = append(coreObjects, pod)
	}
	return &NodeController{
		config: &types.AgentConfig{
			NodeName: testNodeName,
			Storage:  types.StorageSpec{Type: "pvc", BasePath: t.TempDir()},
		},
		clientset:              fake.NewClientset(coreObjects...),
		client:                 clientBuilder.Build(),
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

func restorePod(annotations map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "restore-worker",
			Namespace:   "inference",
			UID:         ktypes.UID("restore-pod-uid"),
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			NodeName:   testNodeName,
			Containers: []corev1.Container{{Name: "main"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: corev1.ConditionFalse,
			}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:        "main",
				Ready:       true,
				ContainerID: "containerd://" + testContainerID,
			}},
		},
	}
}

func readySnapshotObjects() (*snapshotv1alpha1.PodSnapshot, *snapshotv1alpha1.PodSnapshotContent) {
	contentName := "podsnapshotcontent-snapshot-uid"
	snapshot := &snapshotv1alpha1.PodSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snapshot-a", Namespace: "inference", UID: ktypes.UID("snapshot-uid")},
		Status: snapshotv1alpha1.PodSnapshotStatus{
			BoundPodSnapshotContentName: &contentName,
			Conditions: []metav1.Condition{{
				Type: snapshotv1alpha1.PodSnapshotConditionReady, Status: metav1.ConditionTrue,
			}},
		},
	}
	content := &snapshotv1alpha1.PodSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: contentName, UID: ktypes.UID("content-uid")},
		Spec: snapshotv1alpha1.PodSnapshotContentSpec{
			PodSnapshotRef: snapshotv1alpha1.PodSnapshotReference{Namespace: "inference", Name: "snapshot-a", UID: snapshot.UID},
			Source: snapshotv1alpha1.PodSnapshotContentSource{
				PodRef:   snapshotv1alpha1.PodReference{Name: "source", Containers: []string{"main"}},
				NodeName: "source-node",
			},
		},
		Status: snapshotv1alpha1.PodSnapshotContentStatus{Conditions: []metav1.Condition{{
			Type: snapshotv1alpha1.PodSnapshotConditionReady, Status: metav1.ConditionTrue,
		}}},
	}
	return snapshot, content
}

func TestTweakNodePodListOptions(t *testing.T) {
	t.Run("node only", func(t *testing.T) {
		opts := &metav1.ListOptions{}
		tweakNodePodListOptions(testNodeName)(opts)
		assert.Empty(t, opts.LabelSelector)
		assert.Equal(t, "spec.nodeName="+testNodeName, opts.FieldSelector)
	})

	t.Run("label and node", func(t *testing.T) {
		opts := &metav1.ListOptions{}
		tweakLabeledNodePodListOptions("capture-eligible=true", testNodeName)(opts)
		assert.Equal(t, "capture-eligible=true", opts.LabelSelector)
		assert.Equal(t, "spec.nodeName="+testNodeName, opts.FieldSelector)
	})
}

func TestResolveRestoreArtifact(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	snapshot, content := readySnapshotObjects()
	w := makeTestController(t, pod, snapshot, content)
	path, err := nsmount.ResolveArtifactPath(w.config.Storage.BasePath, string(content.UID), "main")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(path, 0o700))
	require.NoError(t, types.WriteManifest(path, &types.CheckpointManifest{
		Artifact: types.ArtifactManifest{ContentUID: string(content.UID), ContainerName: "main"},
	}))

	artifact, pendingReason, _, err := w.resolveRestoreArtifact(context.Background(), pod)
	require.NoError(t, err)
	require.NotNil(t, artifact)
	assert.Empty(t, pendingReason)
	assert.Equal(t, "snapshot-a", artifact.SnapshotName)
	assert.Equal(t, string(content.UID), artifact.ContentUID)
	assert.Equal(t, "main", artifact.ContainerName)
	assert.Equal(t, path, artifact.Path)
}

func TestResolveRestoreArtifactPendingStates(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	t.Run("missing snapshot", func(t *testing.T) {
		w := makeTestController(t, pod)
		artifact, reason, _, err := w.resolveRestoreArtifact(context.Background(), pod)
		require.NoError(t, err)
		assert.Nil(t, artifact)
		assert.Equal(t, "SnapshotPending", reason)
	})
	t.Run("artifact visibility", func(t *testing.T) {
		snapshot, content := readySnapshotObjects()
		w := makeTestController(t, pod, snapshot, content)
		artifact, reason, _, err := w.resolveRestoreArtifact(context.Background(), pod)
		require.NoError(t, err)
		assert.Nil(t, artifact)
		assert.Equal(t, "ArtifactPending", reason)
	})
}

func TestResolveRestoreArtifactValidatesContentBacklink(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	tests := map[string]func(*snapshotv1alpha1.PodSnapshotContent){
		"namespace": func(content *snapshotv1alpha1.PodSnapshotContent) {
			content.Spec.PodSnapshotRef.Namespace = "other"
		},
		"name": func(content *snapshotv1alpha1.PodSnapshotContent) {
			content.Spec.PodSnapshotRef.Name = "other"
		},
		"missing UID": func(content *snapshotv1alpha1.PodSnapshotContent) {
			content.Spec.PodSnapshotRef.UID = ""
		},
		"mismatched UID": func(content *snapshotv1alpha1.PodSnapshotContent) {
			content.Spec.PodSnapshotRef.UID = "other-uid"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			snapshot, content := readySnapshotObjects()
			mutate(content)
			w := makeTestController(t, pod, snapshot, content)
			_, _, _, err := w.resolveRestoreArtifact(context.Background(), pod)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "stale backlink")
		})
	}
}

func TestReconcileRestorePodReportsInvalidBacklinkAsFailedCondition(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	snapshot, content := readySnapshotObjects()
	content.Spec.PodSnapshotRef.UID = "other-uid"
	w := makeTestController(t, pod, snapshot, content)

	w.reconcileRestorePod(context.Background(), pod)

	var statusPatch clientgotesting.PatchAction
	for _, action := range w.clientset.(*fake.Clientset).Actions() {
		patch, ok := action.(clientgotesting.PatchAction)
		if ok && patch.GetSubresource() == "status" {
			statusPatch = patch
		}
	}
	require.NotNil(t, statusPatch)
	payload := string(statusPatch.GetPatch())
	assert.Contains(t, payload, `"type":"Restored"`)
	assert.Contains(t, payload, `"status":"False"`)
	assert.Contains(t, payload, `"reason":"RestoreFailed"`)
}

func TestResolveRestoreArtifactAllowsUnrelatedAnnotations(t *testing.T) {
	pod := restorePod(map[string]string{
		snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a",
		"example.com/team":                     "inference",
	})
	snapshot, content := readySnapshotObjects()
	w := makeTestController(t, pod, snapshot, content)
	path, err := nsmount.ResolveArtifactPath(w.config.Storage.BasePath, string(content.UID), "main")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(path, 0o700))

	artifact, pendingReason, _, err := w.resolveRestoreArtifact(context.Background(), pod)
	require.NoError(t, err)
	require.NotNil(t, artifact)
	assert.Empty(t, pendingReason)
	assert.Equal(t, string(content.UID), artifact.ContentUID)
	assert.Equal(t, "main", artifact.ContainerName)
}

func TestApplyRestoredConditionUsesServerSideApplyStatus(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	w := makeTestController(t, pod)
	require.NoError(t, w.applyRestoredCondition(context.Background(), pod, corev1.ConditionFalse, "SnapshotPending", "waiting"))

	var patch clientgotesting.PatchAction
	for _, action := range w.clientset.(*fake.Clientset).Actions() {
		if candidate, ok := action.(clientgotesting.PatchAction); ok && candidate.GetSubresource() == "status" {
			patch = candidate
			break
		}
	}
	require.NotNil(t, patch)
	assert.Equal(t, ktypes.ApplyPatchType, patch.GetPatchType())
	payload := string(patch.GetPatch())
	assert.Contains(t, payload, `"type":"Restored"`)
	assert.Contains(t, payload, `"reason":"SnapshotPending"`)
	assert.NotContains(t, payload, `"type":"Ready"`)
	assert.NotContains(t, payload, `"annotations"`)
}

func TestApplyRestoredConditionPreservesTransitionTimeForSameStatus(t *testing.T) {
	transition := metav1.NewTime(time.Unix(123, 0))
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type: corev1.PodConditionType(snapshotv1alpha1.RestoredCondition), Status: corev1.ConditionFalse, LastTransitionTime: transition,
	})
	w := makeTestController(t, pod)
	require.NoError(t, w.applyRestoredCondition(context.Background(), pod, corev1.ConditionFalse, "ArtifactPending", "waiting"))

	actions := w.clientset.(*fake.Clientset).Actions()
	patch := actions[len(actions)-1].(clientgotesting.PatchAction)
	assert.Contains(t, string(patch.GetPatch()), transition.UTC().Format(time.RFC3339))
}

func TestConvergeRestoredFailureConditionReappliesAfterContainerExit(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	pod.Status.ContainerStatuses[0].State.Running = &corev1.ContainerStateRunning{}
	w := makeTestController(t, pod)

	originalInterval := restoreFailureStatusRetryInterval
	restoreFailureStatusRetryInterval = time.Millisecond
	t.Cleanup(func() { restoreFailureStatusRetryInterval = originalInterval })

	getCount := 0
	w.clientset.(*fake.Clientset).PrependReactor("get", "pods", func(_ clientgotesting.Action) (bool, runtime.Object, error) {
		getCount++
		livePod := pod.DeepCopy()
		if getCount > 1 {
			livePod.Status.ContainerStatuses[0].State.Running = nil
			livePod.Status.ContainerStatuses[0].State.Terminated = &corev1.ContainerStateTerminated{ExitCode: 137}
		}
		return true, livePod, nil
	})

	require.NoError(t, w.convergeRestoredFailureCondition(context.Background(), pod, "main", restoreFailedReason, errors.New("restore failed")))
	assert.GreaterOrEqual(t, getCount, 2)

	var patches []clientgotesting.PatchAction
	for _, action := range w.clientset.(*fake.Clientset).Actions() {
		if patch, ok := action.(clientgotesting.PatchAction); ok && patch.GetSubresource() == "status" {
			patches = append(patches, patch)
		}
	}
	require.Len(t, patches, 2)
	for _, patch := range patches {
		assert.Equal(t, ktypes.ApplyPatchType, patch.GetPatchType())
		assert.Contains(t, string(patch.GetPatch()), `"reason":"RestoreFailed"`)
	}
}

func TestContainerIsRunning(t *testing.T) {
	pod := restorePod(nil)
	assert.False(t, containerIsRunning(pod, "main"))
	pod.Status.ContainerStatuses[0].State.Running = &corev1.ContainerStateRunning{}
	assert.True(t, containerIsRunning(pod, "main"))
	assert.False(t, containerIsRunning(pod, "other"))
}

func TestRestoreConditionTerminal(t *testing.T) {
	pod := restorePod(nil)
	assert.False(t, restoreConditionTerminal(pod))
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type: corev1.PodConditionType(snapshotv1alpha1.RestoredCondition), Status: corev1.ConditionFalse, Reason: "RestoreInProgress",
	})
	assert.False(t, restoreConditionTerminal(pod))
	pod.Status.Conditions[len(pod.Status.Conditions)-1].Reason = "RestoreFailed"
	assert.True(t, restoreConditionTerminal(pod))
	pod.Status.Conditions[len(pod.Status.Conditions)-1].Reason = "RestoreSucceeded"
	pod.Status.Conditions[len(pod.Status.Conditions)-1].Status = corev1.ConditionTrue
	assert.True(t, restoreConditionTerminal(pod))
}

func TestRunRestoreCleanupFailureStillCompletesRestore(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	w := makeTestController(t, pod)
	artifactPath := t.TempDir()
	artifact := &restoreArtifact{
		SnapshotName:  "snapshot-a",
		ContentUID:    "content-uid",
		ContainerName: "main",
		Path:          artifactPath,
	}

	var request executor.RestoreRequest
	w.restoreFn = func(_ context.Context, _ snapshotruntime.Runtime, _ logr.Logger, got executor.RestoreRequest, _ executor.RestoreMounter) (int, error) {
		request = got
		return 4242, executor.NewRestoreCleanupError(errors.New("unmount checkpoint artifact: unmount failed"))
	}
	var sentinelPID int
	w.writeControlSentinelFn = func(pid int, _ string) error {
		sentinelPID = pid
		return nil
	}

	err := w.runRestore(context.Background(), pod, artifact, "ctr-abc", "inference/restore-worker/main/ctr-abc", time.Time{})
	require.NoError(t, err)
	assert.Equal(t, "content-uid", request.ContentUID)
	assert.Equal(t, w.config.Storage.BasePath, request.BasePath)
	assert.Equal(t, 4242, sentinelPID)
	assert.True(t, sawEventReason(w.clientset.(*fake.Clientset), "RestoreCleanupFailed"))

	var succeededPatch clientgotesting.PatchAction
	for _, action := range w.clientset.(*fake.Clientset).Actions() {
		patch, ok := action.(clientgotesting.PatchAction)
		if ok && patch.GetSubresource() == "status" && strings.Contains(string(patch.GetPatch()), "RestoreSucceeded") {
			succeededPatch = patch
		}
	}
	require.NotNil(t, succeededPatch)
	assert.Contains(t, string(succeededPatch.GetPatch()), `"status":"True"`)
}

func TestRestoreArtifactReady(t *testing.T) {
	w := makeTestController(t, nil)
	ready, err := w.restoreArtifactReady(testr.New(t), "inference/restore-worker", w.config.Storage.BasePath+"/missing")
	require.NoError(t, err)
	assert.False(t, ready)

	file := w.config.Storage.BasePath + "/file"
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	_, err = w.restoreArtifactReady(testr.New(t), "inference/restore-worker", file)
	require.Error(t, err)
}

func TestCheckpointLeaseNameUsesContentAndContainer(t *testing.T) {
	a := checkpointLeaseName("content-uid", "main")
	b := checkpointLeaseName("content-uid", "worker")
	assert.NotEqual(t, a, b)
	assert.True(t, strings.HasPrefix(a, "snapshot-capture-"))
}

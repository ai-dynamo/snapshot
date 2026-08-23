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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
		clientBuilder = clientBuilder.WithRuntimeObjects(pod)
	}
	clientBuilder = clientBuilder.WithStatusSubresource(&corev1.Pod{})
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

func restoredPodCondition(pod *corev1.Pod) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == corev1.PodConditionType(snapshotv1alpha1.RestoredCondition) {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
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

func TestTrimRestorePod(t *testing.T) {
	deleting := metav1.Now()
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	pod.ResourceVersion = "42"
	pod.DeletionTimestamp = &deleting
	pod.Labels = map[string]string{"large": "unused"}
	pod.Spec.Containers[0].Image = "large-image-reference"
	pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "UNUSED", Value: "large"}}
	pod.Status.PodIP = "10.0.0.2"
	pod.Status.ContainerStatuses[0].State.Running = &corev1.ContainerStateRunning{}

	obj, err := trimRestorePod(pod)
	require.NoError(t, err)
	trimmed := obj.(*corev1.Pod)
	assert.Equal(t, pod.Name, trimmed.Name)
	assert.Equal(t, pod.Namespace, trimmed.Namespace)
	assert.Equal(t, pod.UID, trimmed.UID)
	assert.Equal(t, "42", trimmed.ResourceVersion)
	assert.Equal(t, pod.Annotations, trimmed.Annotations)
	assert.Equal(t, testNodeName, trimmed.Spec.NodeName)
	assert.Equal(t, "main", trimmed.Spec.Containers[0].Name)
	assert.Empty(t, trimmed.Spec.Containers[0].Image)
	assert.Empty(t, trimmed.Spec.Containers[0].Env)
	assert.Empty(t, trimmed.Labels)
	assert.Equal(t, corev1.PodRunning, trimmed.Status.Phase)
	assert.Equal(t, "10.0.0.2", trimmed.Status.PodIP)
	assert.Equal(t, testContainerID, snapshotruntime.StripCRIScheme(trimmed.Status.ContainerStatuses[0].ContainerID))
	assert.NotNil(t, trimmed.Status.ContainerStatuses[0].State.Running)
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

func TestResolveRestoreArtifactTerminalStates(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	tests := map[string]struct {
		mutateSnapshot func(*snapshotv1alpha1.PodSnapshot)
		mutateContent  func(*snapshotv1alpha1.PodSnapshotContent)
		mutatePod      func(*corev1.Pod)
		want           string
	}{
		"snapshot failed": {
			mutateSnapshot: func(snapshot *snapshotv1alpha1.PodSnapshot) {
				snapshot.Status.Conditions = []metav1.Condition{{
					Type: snapshotv1alpha1.PodSnapshotConditionFailed, Status: metav1.ConditionTrue, Message: "dump aborted",
				}}
			},
			want: "dump aborted",
		},
		"content failed": {
			mutateContent: func(content *snapshotv1alpha1.PodSnapshotContent) {
				content.Status.Conditions = []metav1.Condition{{
					Type: snapshotv1alpha1.PodSnapshotConditionFailed, Status: metav1.ConditionTrue, Message: "criu failed",
				}}
			},
			want: "criu failed",
		},
		"missing captured container": {
			mutatePod: func(target *corev1.Pod) {
				target.Spec.Containers = []corev1.Container{{Name: "sidecar"}}
			},
			want: `has no container named "main"`,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			target := pod.DeepCopy()
			snapshot, content := readySnapshotObjects()
			if tc.mutateSnapshot != nil {
				tc.mutateSnapshot(snapshot)
			}
			if tc.mutateContent != nil {
				tc.mutateContent(content)
			}
			if tc.mutatePod != nil {
				tc.mutatePod(target)
			}
			w := makeTestController(t, target, snapshot, content)
			artifact, _, _, err := w.resolveRestoreArtifact(context.Background(), target)
			require.Error(t, err)
			assert.Nil(t, artifact)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestReconcileRestorePodReportsInvalidBacklinkAsFailedCondition(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	snapshot, content := readySnapshotObjects()
	content.Spec.PodSnapshotRef.UID = "other-uid"
	w := makeTestController(t, pod, snapshot, content)

	w.reconcileRestorePod(context.Background(), pod)

	got := &corev1.Pod{}
	require.NoError(t, w.client.Get(context.Background(), client.ObjectKeyFromObject(pod), got))
	condition := restoredPodCondition(got)
	require.NotNil(t, condition)
	assert.Equal(t, corev1.ConditionFalse, condition.Status)
	assert.Equal(t, restoreFailedReason, condition.Reason)
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

func TestApplyRestoredConditionUsesOptimisticStatusPatch(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	w := makeTestController(t, pod)
	proceed, err := w.applyRestoredCondition(context.Background(), pod, corev1.ConditionFalse, "SnapshotPending", "waiting")
	require.NoError(t, err)
	assert.True(t, proceed)

	got := &corev1.Pod{}
	require.NoError(t, w.client.Get(context.Background(), client.ObjectKeyFromObject(pod), got))
	condition := restoredPodCondition(got)
	require.NotNil(t, condition)
	assert.Equal(t, corev1.ConditionFalse, condition.Status)
	assert.Equal(t, "SnapshotPending", condition.Reason)
	assert.Equal(t, "waiting", condition.Message)
	assert.Equal(t, corev1.PodReady, got.Status.Conditions[0].Type)
	assert.Equal(t, corev1.ConditionFalse, got.Status.Conditions[0].Status)
}

func TestApplyRestoredConditionPreservesTransitionTimeForSameStatus(t *testing.T) {
	transition := metav1.NewTime(time.Unix(123, 0))
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type: corev1.PodConditionType(snapshotv1alpha1.RestoredCondition), Status: corev1.ConditionFalse, LastTransitionTime: transition,
	})
	w := makeTestController(t, pod)
	_, err := w.applyRestoredCondition(context.Background(), pod, corev1.ConditionFalse, "ArtifactPending", "waiting")
	require.NoError(t, err)

	got := &corev1.Pod{}
	require.NoError(t, w.client.Get(context.Background(), client.ObjectKeyFromObject(pod), got))
	require.NotNil(t, restoredPodCondition(got))
	assert.Equal(t, transition, restoredPodCondition(got).LastTransitionTime)
}

func TestApplyRestoredConditionIgnoresAlreadyRestoredPod(t *testing.T) {
	livePod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	livePod.Status.Conditions = append(livePod.Status.Conditions, corev1.PodCondition{
		Type: corev1.PodConditionType(snapshotv1alpha1.RestoredCondition), Status: corev1.ConditionTrue, Reason: "RestoreSucceeded",
	})
	stalePod := livePod.DeepCopy()
	stalePod.Status.Conditions = stalePod.Status.Conditions[:1]
	w := makeTestController(t, livePod)

	proceed, err := w.applyRestoredCondition(context.Background(), stalePod, corev1.ConditionFalse, "RestoreInProgress", "stale")
	require.NoError(t, err)
	assert.False(t, proceed)

	got := &corev1.Pod{}
	require.NoError(t, w.client.Get(context.Background(), client.ObjectKeyFromObject(livePod), got))
	condition := restoredPodCondition(got)
	require.NotNil(t, condition)
	assert.Equal(t, corev1.ConditionTrue, condition.Status)
	assert.Equal(t, "RestoreSucceeded", condition.Reason)
}

func TestApplyRestoredConditionRetriesConflict(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	w := makeTestController(t, pod)
	patchAttempts := 0
	w.client = ctrlfake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithRuntimeObjects(pod).
		WithStatusSubresource(&corev1.Pod{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, delegated client.Client, subresource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				patchAttempts++
				if patchAttempts == 1 {
					return apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, pod.Name, errors.New("conflict"))
				}
				return delegated.SubResource(subresource).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	proceed, err := w.applyRestoredCondition(context.Background(), pod, corev1.ConditionFalse, "RestoreInProgress", "retry")
	require.NoError(t, err)
	assert.True(t, proceed)
	assert.Equal(t, 2, patchAttempts)
}

func TestRestoreFailureReleasesAttemptForRetry(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	w := makeTestController(t, pod)
	key := "inference/restore-worker/main/ctr-abc"
	require.True(t, w.tryAcquire(key))
	op := w.newRestoreOperation(pod, &restoreArtifact{SnapshotName: "snapshot-a", ContainerName: "main"}, "ctr-abc", key, time.Time{})

	proceed, err := op.setRestoredCondition(context.Background(), corev1.ConditionFalse, restoreFailedReason, "failed")
	require.NoError(t, err)
	assert.True(t, proceed)
	op.release()
	assert.True(t, w.tryAcquire(key), "the same container attempt remains retryable after RestoreFailed")
	w.release(key)
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

	got := &corev1.Pod{}
	require.NoError(t, w.client.Get(context.Background(), client.ObjectKeyFromObject(pod), got))
	condition := restoredPodCondition(got)
	require.NotNil(t, condition)
	assert.Equal(t, restoreFailedReason, condition.Reason)
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
	assert.False(t, restoreConditionTerminal(pod))
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

	got := &corev1.Pod{}
	require.NoError(t, w.client.Get(context.Background(), client.ObjectKeyFromObject(pod), got))
	condition := restoredPodCondition(got)
	require.NotNil(t, condition)
	assert.Equal(t, corev1.ConditionTrue, condition.Status)
	assert.Equal(t, "RestoreSucceeded", condition.Reason)
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

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"syscall"
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
	corev1listers "k8s.io/client-go/listers/core/v1"
	clientgotesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	clocktesting "k8s.io/utils/clock/testing"
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
	t.Cleanup(w.restoreQueue.ShutDown)
	if w.checkpointFn == nil || w.restoreFn == nil || w.writeControlSentinelFn == nil || w.controlSentinelExistsFn == nil || w.sendSignalFn == nil || w.releaseCheckpointFn == nil || w.restoreQueue == nil {
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
	w := &NodeController{
		config: &types.AgentConfig{
			NodeName: testNodeName,
			Storage:  types.StorageSpec{Type: "pvc", BasePath: t.TempDir()},
		},
		clientset:               fake.NewClientset(coreObjects...),
		client:                  clientBuilder.Build(),
		runtime:                 &fakeRuntime{},
		injector:                noopInjector{},
		restoreFn:               executor.Restore,
		writeControlSentinelFn:  func(int, string) error { return nil },
		controlSentinelExistsFn: func(int, string) (bool, error) { return false, nil },
		sendSignalFn:            func(logr.Logger, int, syscall.Signal, string) error { return nil },
		restoreQueue:            workqueue.NewTypedDelayingQueue[client.ObjectKey](),
		log:                     testr.New(t),
		holderID:                "test-holder",
		inFlight:                make(map[string]struct{}),
		stopCh:                  make(chan struct{}),
	}
	t.Cleanup(w.restoreQueue.ShutDown)
	return w
}

func lastPodStatusApply(t *testing.T, w *NodeController) clientgotesting.PatchAction {
	t.Helper()
	actions := w.clientset.(*fake.Clientset).Actions()
	for i := len(actions) - 1; i >= 0; i-- {
		patch, ok := actions[i].(clientgotesting.PatchAction)
		if ok && patch.GetSubresource() == "status" {
			return patch
		}
	}
	t.Fatal("no Pod status apply action found")
	return nil
}

func hasPodStatusApply(w *NodeController) bool {
	for _, action := range w.clientset.(*fake.Clientset).Actions() {
		if patch, ok := action.(clientgotesting.PatchAction); ok && patch.GetSubresource() == "status" {
			return true
		}
	}
	return false
}

func sawEventReason(clientset *fake.Clientset, reason string) bool {
	return eventForReason(clientset, reason) != nil
}

func eventForReason(clientset *fake.Clientset, reason string) *corev1.Event {
	for _, action := range clientset.Actions() {
		create, ok := action.(clientgotesting.CreateAction)
		if !ok || create.GetResource().Resource != "events" {
			continue
		}
		event, ok := create.GetObject().(*corev1.Event)
		if ok && event.Reason == reason {
			return event
		}
	}
	return nil
}

func pendingRestoreReason(t *testing.T, err error) string {
	t.Helper()
	var pending *restorePendingError
	require.ErrorAs(t, err, &pending)
	return pending.reason
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
				State:       corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
}

func processQueuedRestorePod(t *testing.T, w *NodeController, pod *corev1.Pod) {
	t.Helper()
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	require.NoError(t, indexer.Add(pod))
	w.restorePodLister = corev1listers.NewPodLister(indexer)
	w.restoreQueue.Add(client.ObjectKeyFromObject(pod))
	item, shutdown := w.restoreQueue.Get()
	require.False(t, shutdown)
	w.processRestoreQueueItem(context.Background(), item)
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

func TestEnqueueRestorePodFiltersAndDeduplicatesEvents(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	w := makeTestController(t, pod)
	t.Cleanup(w.restoreQueue.ShutDown)

	withoutRestore := pod.DeepCopy()
	withoutRestore.Annotations = nil
	w.enqueueRestorePod(withoutRestore)

	wrongNode := pod.DeepCopy()
	wrongNode.Spec.NodeName = "other-node"
	w.enqueueRestorePod(wrongNode)
	assert.Zero(t, w.restoreQueue.Len())

	w.enqueueRestorePod(pod)
	w.enqueueRestorePod(pod.DeepCopy())
	assert.Equal(t, 1, w.restoreQueue.Len())
}

func TestRestoreQueueRequeuesDirtyKeyAfterDone(t *testing.T) {
	w := makeTestController(t, restorePod(nil))
	t.Cleanup(w.restoreQueue.ShutDown)
	key := client.ObjectKey{Namespace: "inference", Name: "restore-worker"}

	w.restoreQueue.Add(key)
	item, shutdown := w.restoreQueue.Get()
	require.False(t, shutdown)
	assert.Equal(t, key, item)

	w.restoreQueue.Add(key)
	assert.Zero(t, w.restoreQueue.Len(), "an in-progress key must not run concurrently")
	w.restoreQueue.Done(item)
	assert.Equal(t, 1, w.restoreQueue.Len(), "an update during processing must run after Done")

	item, shutdown = w.restoreQueue.Get()
	require.False(t, shutdown)
	w.restoreQueue.Done(item)
}

func TestProcessRestoreQueueItemUsesCachedPod(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	w := makeTestController(t, pod)
	w.restoreQueue.ShutDown()
	testClock := clocktesting.NewFakeClock(time.Now())
	w.restoreQueue = workqueue.NewTypedDelayingQueueWithConfig(workqueue.TypedDelayingQueueConfig[client.ObjectKey]{Clock: testClock})
	t.Cleanup(w.restoreQueue.ShutDown)
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	require.NoError(t, indexer.Add(pod))
	w.restorePodLister = corev1listers.NewPodLister(indexer)
	key := client.ObjectKeyFromObject(pod)
	w.restoreQueue.Add(key)
	item, shutdown := w.restoreQueue.Get()
	require.False(t, shutdown)

	w.processRestoreQueueItem(context.Background(), item)

	assert.Contains(t, string(lastPodStatusApply(t, w).GetPatch()), `"reason":"SnapshotPending"`)
	assert.Zero(t, w.restoreQueue.Len())
	testClock.Step(restoreSafetyRequeueInterval)
	assert.Eventually(t, func() bool { return w.restoreQueue.Len() == 1 }, time.Second, time.Millisecond)
}

func TestPreflightRestore(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	snapshot, content := readySnapshotObjects()
	w := makeTestController(t, pod, snapshot, content)
	path, err := nsmount.ResolveArtifactPath(w.config.Storage.BasePath, string(content.UID), "main")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(path, 0o700))
	require.NoError(t, types.WriteManifest(path, &types.CheckpointManifest{
		Artifact: types.ArtifactManifest{ContentUID: string(content.UID), ContainerName: "main"},
	}))

	artifact, err := w.preflightRestore(context.Background(), pod)
	require.NoError(t, err)
	require.NotNil(t, artifact)
	assert.Equal(t, "snapshot-a", artifact.SnapshotName)
	assert.Equal(t, string(content.UID), artifact.ContentUID)
	assert.Equal(t, "main", artifact.ContainerName)
	assert.Equal(t, path, artifact.Path)
}

func TestPreflightRestoreRetriesInProgressCondition(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type: corev1.PodConditionType(snapshotv1alpha1.RestoredCondition), Status: corev1.ConditionFalse, Reason: "RestoreInProgress",
	})
	snapshot, content := readySnapshotObjects()
	w := makeTestController(t, pod, snapshot, content)
	path, err := nsmount.ResolveArtifactPath(w.config.Storage.BasePath, string(content.UID), "main")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(path, 0o700))

	artifact, err := w.preflightRestore(context.Background(), pod)

	require.NoError(t, err)
	require.NotNil(t, artifact)
	assert.Equal(t, path, artifact.Path)
}

func TestPreflightRestorePendingStates(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	t.Run("missing snapshot", func(t *testing.T) {
		w := makeTestController(t, pod)
		artifact, err := w.preflightRestore(context.Background(), pod)
		assert.Nil(t, artifact)
		assert.Equal(t, "SnapshotPending", pendingRestoreReason(t, err))
	})
	t.Run("missing content", func(t *testing.T) {
		snapshot, _ := readySnapshotObjects()
		w := makeTestController(t, pod, snapshot)
		artifact, err := w.preflightRestore(context.Background(), pod)
		assert.Nil(t, artifact)
		assert.Equal(t, "ContentPending", pendingRestoreReason(t, err))
	})
	t.Run("content not ready", func(t *testing.T) {
		snapshot, content := readySnapshotObjects()
		content.Status.Conditions = nil
		w := makeTestController(t, pod, snapshot, content)
		artifact, err := w.preflightRestore(context.Background(), pod)
		assert.Nil(t, artifact)
		assert.Equal(t, "ContentPending", pendingRestoreReason(t, err))
	})
	t.Run("artifact visibility", func(t *testing.T) {
		snapshot, content := readySnapshotObjects()
		w := makeTestController(t, pod, snapshot, content)
		artifact, err := w.preflightRestore(context.Background(), pod)
		assert.Nil(t, artifact)
		assert.Equal(t, "ArtifactPending", pendingRestoreReason(t, err))
	})
}

func TestPreflightRestoreFailsWhenInProgressSnapshotDisappears(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type: corev1.PodConditionType(snapshotv1alpha1.RestoredCondition), Status: corev1.ConditionFalse, Reason: restoreInProgressReason,
	})
	w := makeTestController(t, pod)

	artifact, err := w.preflightRestore(context.Background(), pod)

	assert.Nil(t, artifact)
	require.Error(t, err)
	var pending *restorePendingError
	assert.False(t, errors.As(err, &pending))
	assert.Contains(t, err.Error(), "disappeared while restore was in progress")
}

func TestRestorePreflightAPIErrorsUseStablePendingMessages(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	snapshot, _ := readySnapshotObjects()

	tests := []struct {
		name        string
		objects     []runtime.Object
		failType    client.Object
		wantReason  string
		wantMessage string
	}{
		{
			name:        "snapshot read",
			failType:    &snapshotv1alpha1.PodSnapshot{},
			wantReason:  "SnapshotPending",
			wantMessage: "Unable to read PodSnapshot inference/snapshot-a; retrying",
		},
		{
			name:        "content read",
			objects:     []runtime.Object{snapshot},
			failType:    &snapshotv1alpha1.PodSnapshotContent{},
			wantReason:  "ContentPending",
			wantMessage: "Unable to read bound PodSnapshotContent podsnapshotcontent-snapshot-uid; retrying",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := makeTestController(t, pod)
			w.client = ctrlfake.NewClientBuilder().
				WithScheme(testScheme(t)).
				WithRuntimeObjects(tc.objects...).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, delegated client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if reflect.TypeOf(obj) == reflect.TypeOf(tc.failType) {
							return errors.New("transient apiserver detail")
						}
						return delegated.Get(ctx, key, obj, opts...)
					},
				}).
				Build()

			artifact, err := w.preflightRestore(context.Background(), pod)

			assert.Nil(t, artifact)
			assert.Equal(t, tc.wantReason, pendingRestoreReason(t, err))
			assert.EqualError(t, err, tc.wantMessage)
			assert.NotContains(t, err.Error(), "transient apiserver detail")
		})
	}
}

func TestPreflightRestoreWaitsForPodIPBeforeTCPRestore(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	w := makeTestController(t, pod)
	w.config.CRIU.TcpEstablished = true

	artifact, err := w.preflightRestore(context.Background(), pod)

	assert.Nil(t, artifact)
	assert.Equal(t, "PodIPPending", pendingRestoreReason(t, err))
}

func TestReconcileRestorePodReportsPendingPreflightConditionAndEvent(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	w := makeTestController(t, pod)

	requeue := w.reconcileRestorePod(context.Background(), pod)

	assert.True(t, requeue)
	payload := string(lastPodStatusApply(t, w).GetPatch())
	assert.Contains(t, payload, `"status":"False"`)
	assert.Contains(t, payload, `"reason":"SnapshotPending"`)
	assert.True(t, sawEventReason(w.clientset.(*fake.Clientset), "SnapshotPending"))
}

func TestPendingDependencyDoesNotOverwriteRestoreInProgress(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type: corev1.PodConditionType(snapshotv1alpha1.RestoredCondition), Status: corev1.ConditionFalse, Reason: restoreInProgressReason,
	})
	w := makeTestController(t, pod)

	requeue := w.handleRestorePreflightError(context.Background(), pod, newRestorePendingError("ArtifactPending", "waiting"))

	assert.True(t, requeue)
	assert.False(t, hasPodStatusApply(w))
	assert.Equal(t, restoreInProgressReason, restoredPodCondition(pod).Reason)
	assert.True(t, sawEventReason(w.clientset.(*fake.Clientset), "ArtifactPending"))
}

func TestProcessRestoreQueueItemReportsNonRunningPhaseAsFailed(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	pod.Status.Phase = corev1.PodFailed
	w := makeTestController(t, pod)

	processQueuedRestorePod(t, w, pod)

	payload := string(lastPodStatusApply(t, w).GetPatch())
	assert.Contains(t, payload, `"reason":"RestoreFailed"`)
	assert.Contains(t, payload, "phase Failed")
	assert.True(t, sawEventReason(w.clientset.(*fake.Clientset), restoreFailedReason))
}

func TestContainerPollingDoesNotSetInProgressBeforeExecution(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	pod.Status.ContainerStatuses[0].ContainerID = ""
	snapshot, content := readySnapshotObjects()
	w := makeTestController(t, pod, snapshot, content)
	path, err := nsmount.ResolveArtifactPath(w.config.Storage.BasePath, string(content.UID), "main")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(path, 0o700))
	artifact, err := w.preflightRestore(context.Background(), pod)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	containerID, requeue := w.resolveRestoreContainerID(ctx, pod, artifact, "inference/restore-worker")

	assert.Empty(t, containerID)
	assert.False(t, requeue)
	assert.False(t, hasPodStatusApply(w))
}

func TestPreflightRestoreValidatesContentBacklink(t *testing.T) {
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
			_, err := w.preflightRestore(context.Background(), pod)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "stale backlink")
		})
	}
}

func TestPreflightRestoreTerminalStates(t *testing.T) {
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
			artifact, err := w.preflightRestore(context.Background(), target)
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

	payload := string(lastPodStatusApply(t, w).GetPatch())
	assert.Contains(t, payload, `"status":"False"`)
	assert.Contains(t, payload, `"reason":"RestoreFailed"`)
}

func TestProcessRestoreQueueItemEmitsEventWhenRestoreAlreadyCompleted(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	pod.Finalizers = []string{restorePodFinalizer}
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type: corev1.PodConditionType(snapshotv1alpha1.RestoredCondition), Status: corev1.ConditionTrue, Reason: "RestoreSucceeded",
	})
	w := makeTestController(t, pod)

	processQueuedRestorePod(t, w, pod)

	assert.True(t, sawEventReason(w.clientset.(*fake.Clientset), "RestoreAlreadyCompleted"))
	live, err := w.clientset.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.False(t, hasFinalizer(live, restorePodFinalizer))
}

func TestProcessRestoreQueueItemIgnoresFailedRestoreDuringPreflight(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	pod.Finalizers = []string{restorePodFinalizer}
	pod.Status.Phase = corev1.PodFailed
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type:    corev1.PodConditionType(snapshotv1alpha1.RestoredCondition),
		Status:  corev1.ConditionFalse,
		Reason:  restoreFailedReason,
		Message: "original restore failure",
	})
	w := makeTestController(t, pod)
	getCalls := 0
	w.client = ctrlfake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithRuntimeObjects(pod).
		WithStatusSubresource(&corev1.Pod{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, delegated client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				getCalls++
				return delegated.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	processQueuedRestorePod(t, w, pod)

	assert.Zero(t, getCalls, "failed restore preflight must not read PodSnapshot or PodSnapshotContent")
	condition := restoredPodCondition(pod)
	require.NotNil(t, condition)
	assert.Equal(t, restoreFailedReason, condition.Reason)
	assert.Equal(t, "original restore failure", condition.Message)
	event := eventForReason(w.clientset.(*fake.Clientset), "RestoreAlreadyFailed")
	require.NotNil(t, event)
	assert.Equal(t, corev1.EventTypeWarning, event.Type)
	assert.Equal(t, "original restore failure", event.Message)
	live, err := w.clientset.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.False(t, hasFinalizer(live, restorePodFinalizer))
}

func TestDeletingInProgressRestoreRemovesFinalizer(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	pod.Finalizers = []string{restorePodFinalizer}
	now := metav1.Now()
	pod.DeletionTimestamp = &now
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type: corev1.PodConditionType(snapshotv1alpha1.RestoredCondition), Status: corev1.ConditionFalse, Reason: restoreInProgressReason,
	})
	w := makeTestController(t, pod)

	processQueuedRestorePod(t, w, pod)

	live, err := w.clientset.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.False(t, hasFinalizer(live, restorePodFinalizer))
	assert.False(t, hasPodStatusApply(w))
}

func TestReconcileRestorePodEmitsEventWhenStatusUpdateFails(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	w := makeTestController(t, pod)
	w.clientset.(*fake.Clientset).PrependReactor("patch", "pods", func(clientgotesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("status patch failed")
	})

	requeue := w.reconcileRestorePod(context.Background(), pod)

	assert.True(t, requeue)
	assert.True(t, sawEventReason(w.clientset.(*fake.Clientset), "RestoreStatusUpdateFailed"))
}

func TestPreflightRestoreAllowsUnrelatedAnnotations(t *testing.T) {
	pod := restorePod(map[string]string{
		snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a",
		"example.com/team":                     "inference",
	})
	snapshot, content := readySnapshotObjects()
	w := makeTestController(t, pod, snapshot, content)
	path, err := nsmount.ResolveArtifactPath(w.config.Storage.BasePath, string(content.UID), "main")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(path, 0o700))

	artifact, err := w.preflightRestore(context.Background(), pod)
	require.NoError(t, err)
	require.NotNil(t, artifact)
	assert.Equal(t, string(content.UID), artifact.ContentUID)
	assert.Equal(t, "main", artifact.ContainerName)
}

func TestReconcileRestorePodRunsPreflightOnce(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	snapshot, content := readySnapshotObjects()
	w := makeTestController(t, pod, snapshot, content)
	path, err := nsmount.ResolveArtifactPath(w.config.Storage.BasePath, string(content.UID), "main")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(path, 0o700))
	getCalls := 0
	w.client = ctrlfake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithRuntimeObjects(pod, snapshot, content).
		WithStatusSubresource(&corev1.Pod{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, delegated client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				getCalls++
				return delegated.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	w.restoreFn = func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (int, error) {
		return 4242, nil
	}

	requeue := w.reconcileRestorePod(context.Background(), pod)

	assert.False(t, requeue)
	assert.Equal(t, 2, getCalls, "preflight should read PodSnapshot and PodSnapshotContent once")
}

func TestRestoreFinalizerIsPatchedBeforePreflight(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "missing-snapshot"})
	w := makeTestController(t, pod)

	processQueuedRestorePod(t, w, pod)

	live, err := w.clientset.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.True(t, hasFinalizer(live, restorePodFinalizer))
	for _, action := range w.clientset.(*fake.Clientset).Actions() {
		if update, ok := action.(clientgotesting.UpdateAction); ok && update.GetResource().Resource == "pods" {
			t.Fatal("restore finalizer must be patched, not updated")
		}
		patch, ok := action.(clientgotesting.PatchAction)
		if ok && patch.GetSubresource() == "" {
			assert.Equal(t, ktypes.MergePatchType, patch.GetPatchType())
			return
		}
	}
	t.Fatal("no Pod finalizer patch found")
}

func TestRestoreFinalizerProtectsExecutionAndIsRemovedAfterSuccess(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	snapshot, content := readySnapshotObjects()
	w := makeTestController(t, pod, snapshot, content)
	path, err := nsmount.ResolveArtifactPath(w.config.Storage.BasePath, string(content.UID), "main")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(path, 0o700))
	restoreCalls := 0
	w.restoreFn = func(ctx context.Context, _ snapshotruntime.Runtime, _ logr.Logger, _ executor.RestoreRequest, _ executor.RestoreMounter) (int, error) {
		restoreCalls++
		live, getErr := w.clientset.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		require.NoError(t, getErr)
		assert.True(t, hasFinalizer(live, restorePodFinalizer))
		return 4242, nil
	}

	processQueuedRestorePod(t, w, pod)

	assert.Equal(t, 1, restoreCalls)
	live, err := w.clientset.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.False(t, hasFinalizer(live, restorePodFinalizer))
	assert.Contains(t, string(lastPodStatusApply(t, w).GetPatch()), `"reason":"RestoreSucceeded"`)
}

func TestRestoreStatusRetryUsesCompletionSentinelWithoutReplayingRestore(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	snapshot, content := readySnapshotObjects()
	w := makeTestController(t, pod, snapshot, content)
	w.runtime = &fakeRuntime{resolveContainerPID: 4242}
	path, err := nsmount.ResolveArtifactPath(w.config.Storage.BasePath, string(content.UID), "main")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(path, 0o700))

	restoreCalls := 0
	w.restoreFn = func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (int, error) {
		restoreCalls++
		return 4242, nil
	}
	sentinelWritten := false
	w.writeControlSentinelFn = func(pid int, name string) error {
		assert.Equal(t, 4242, pid)
		assert.Equal(t, snapshotv1alpha1.RestoreCompleteFile, name)
		sentinelWritten = true
		return nil
	}
	w.controlSentinelExistsFn = func(pid int, name string) (bool, error) {
		assert.Equal(t, 4242, pid)
		assert.Equal(t, snapshotv1alpha1.RestoreCompleteFile, name)
		return sentinelWritten, nil
	}
	statusPatches := 0
	w.clientset.(*fake.Clientset).PrependReactor("patch", "pods", func(action clientgotesting.Action) (bool, runtime.Object, error) {
		patch := action.(clientgotesting.PatchAction)
		if patch.GetSubresource() != "status" {
			return false, nil, nil
		}
		statusPatches++
		if statusPatches == 2 {
			return true, nil, errors.New("terminal status failed")
		}
		return false, nil, nil
	})

	processQueuedRestorePod(t, w, pod)
	assert.Equal(t, 1, restoreCalls)
	assert.False(t, w.restoreHandled(pod))
	assert.True(t, sawEventReason(w.clientset.(*fake.Clientset), restoreStatusUpdateFailedReason))

	live, err := w.clientset.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.True(t, hasFinalizer(live, restorePodFinalizer))
	condition := restoredPodCondition(live)
	require.NotNil(t, condition)
	assert.Equal(t, restoreInProgressReason, condition.Reason)

	processQueuedRestorePod(t, w, live)
	assert.Equal(t, 1, restoreCalls, "completion sentinel must prevent CRIU replay")
	assert.True(t, w.restoreHandled(live))

	live, err = w.clientset.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.False(t, hasFinalizer(live, restorePodFinalizer))
	condition = restoredPodCondition(live)
	require.NotNil(t, condition)
	assert.Equal(t, corev1.ConditionTrue, condition.Status)
	assert.Equal(t, restoreSucceededReason, condition.Reason)
}

func TestRestoreFinalizerRemovalRetriesWithoutReplayingRestore(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	snapshot, content := readySnapshotObjects()
	w := makeTestController(t, pod, snapshot, content)
	path, err := nsmount.ResolveArtifactPath(w.config.Storage.BasePath, string(content.UID), "main")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(path, 0o700))
	restoreCalls := 0
	w.restoreFn = func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (int, error) {
		restoreCalls++
		return 4242, nil
	}
	metadataPatches := 0
	w.clientset.(*fake.Clientset).PrependReactor("patch", "pods", func(action clientgotesting.Action) (bool, runtime.Object, error) {
		patch := action.(clientgotesting.PatchAction)
		if patch.GetSubresource() != "" {
			return false, nil, nil
		}
		metadataPatches++
		if metadataPatches == 2 {
			return true, nil, errors.New("finalizer update failed")
		}
		return false, nil, nil
	})

	processQueuedRestorePod(t, w, pod)
	assert.Equal(t, 1, restoreCalls)
	assert.True(t, sawEventReason(w.clientset.(*fake.Clientset), restoreFinalizerUpdateFailedReason))

	live, err := w.clientset.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	processQueuedRestorePod(t, w, live)
	assert.Equal(t, 1, restoreCalls)
	live, err = w.clientset.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.False(t, hasFinalizer(live, restorePodFinalizer))
}

func TestApplyRestoredConditionUsesServerSideApply(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	w := makeTestController(t, pod)
	err := w.applyRestoredCondition(context.Background(), pod, corev1.ConditionFalse, "SnapshotPending", "waiting")
	require.NoError(t, err)

	patch := lastPodStatusApply(t, w)
	assert.Equal(t, ktypes.ApplyPatchType, patch.GetPatchType())
	payload := string(patch.GetPatch())
	assert.Contains(t, payload, `"type":"snapshot/Restored"`)
	assert.Contains(t, payload, `"reason":"SnapshotPending"`)
	assert.Contains(t, payload, `"message":"waiting"`)
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
	err := w.applyRestoredCondition(context.Background(), pod, corev1.ConditionFalse, "ArtifactPending", "waiting")
	require.NoError(t, err)

	assert.Contains(t, string(lastPodStatusApply(t, w).GetPatch()), transition.UTC().Format(time.RFC3339))
}

func TestInFlightKeyIsDeduplicatedForCapture(t *testing.T) {
	w := makeTestController(t, restorePod(nil))
	key := "inference/restore-worker/main/ctr-abc"

	assert.True(t, w.tryAcquire(key))
	assert.False(t, w.tryAcquire(key))
	w.release(key)
	assert.True(t, w.tryAcquire(key))
	w.release(key)
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

	requeue, err := w.runRestore(context.Background(), pod, artifact, "ctr-abc", time.Time{})
	require.NoError(t, err)
	assert.False(t, requeue)
	assert.Equal(t, "content-uid", request.ContentUID)
	assert.Equal(t, w.config.Storage.BasePath, request.BasePath)
	assert.Equal(t, 4242, sentinelPID)
	assert.True(t, sawEventReason(w.clientset.(*fake.Clientset), "RestoreCleanupFailed"))

	assert.Contains(t, string(lastPodStatusApply(t, w).GetPatch()), `"reason":"RestoreSucceeded"`)
}

func TestRunRestoreRetriesFullRestoreUntilFailureCleanupSucceeds(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	w := makeTestController(t, pod)
	w.runtime = &fakeRuntime{resolveContainerPID: 4242}
	artifact := &restoreArtifact{SnapshotName: "snapshot-a", ContentUID: "content-uid", ContainerName: "main"}
	restoreCalls := 0
	w.restoreFn = func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (int, error) {
		restoreCalls++
		return 0, errors.New("criu restore failed")
	}
	signalCalls := 0
	w.sendSignalFn = func(logr.Logger, int, syscall.Signal, string) error {
		signalCalls++
		if signalCalls == 1 {
			return errors.New("placeholder still busy")
		}
		return nil
	}

	requeue, err := w.runRestore(context.Background(), pod, artifact, "ctr-abc", time.Time{})
	require.Error(t, err)
	assert.True(t, requeue)
	assert.Equal(t, 1, restoreCalls)
	assert.False(t, w.restoreHandled(pod), "terminal failure must not be cached before cleanup succeeds")

	requeue, err = w.runRestore(context.Background(), pod, artifact, "ctr-abc", time.Time{})
	require.NoError(t, err)
	assert.False(t, requeue)
	assert.Equal(t, 2, restoreCalls, "CRIU restore should retry when the previous cleanup did not finish")
	assert.Contains(t, string(lastPodStatusApply(t, w).GetPatch()), `"reason":"RestoreFailed"`)
}

func TestRestoreFailureStatusErrorRemainsRetryableAfterCleanup(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	pod.Finalizers = []string{restorePodFinalizer}
	w := makeTestController(t, pod)
	w.runtime = &fakeRuntime{resolveContainerPID: 4242}
	artifact := &restoreArtifact{SnapshotName: "snapshot-a", ContentUID: "content-uid", ContainerName: "main"}
	restoreCalls := 0
	w.restoreFn = func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (int, error) {
		restoreCalls++
		return 0, errors.New("criu restore failed")
	}
	patches := 0
	w.clientset.(*fake.Clientset).PrependReactor("patch", "pods", func(clientgotesting.Action) (bool, runtime.Object, error) {
		patches++
		if patches == 2 {
			return true, nil, errors.New("terminal status failed")
		}
		return false, nil, nil
	})

	requeue, err := w.runRestore(context.Background(), pod, artifact, "ctr-abc", time.Time{})
	require.Error(t, err)
	assert.True(t, requeue)
	assert.Equal(t, 1, restoreCalls)
	assert.False(t, w.restoreHandled(pod), "restore must remain retryable until terminal status is persisted")
	live, getErr := w.clientset.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, getErr)
	assert.True(t, hasFinalizer(live, restorePodFinalizer), "restore protection must remain until terminal status is persisted")
}

func TestRunRestoreFinalizesExistingCompletionSentinelWithoutReplay(t *testing.T) {
	pod := restorePod(map[string]string{snapshotv1alpha1.RestoreFromAnnotation: "snapshot-a"})
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type:   corev1.PodConditionType(snapshotv1alpha1.RestoredCondition),
		Status: corev1.ConditionFalse,
		Reason: restoreInProgressReason,
	})
	w := makeTestController(t, pod)
	w.runtime = &fakeRuntime{resolveContainerPID: 4242}
	w.controlSentinelExistsFn = func(pid int, name string) (bool, error) {
		assert.Equal(t, 4242, pid)
		assert.Equal(t, snapshotv1alpha1.RestoreCompleteFile, name)
		return true, nil
	}
	w.restoreFn = func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (int, error) {
		t.Fatal("restore executor must not be replayed after the completion sentinel exists")
		return 0, nil
	}
	artifact := &restoreArtifact{SnapshotName: "snapshot-a", ContentUID: "content-uid", ContainerName: "main"}

	requeue, err := w.runRestore(context.Background(), pod, artifact, "ctr-abc", time.Time{})
	require.NoError(t, err)
	assert.False(t, requeue)

	assert.Contains(t, string(lastPodStatusApply(t, w).GetPatch()), `"reason":"RestoreSucceeded"`)
	assert.True(t, sawEventReason(w.clientset.(*fake.Clientset), restoreSucceededReason))
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

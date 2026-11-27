// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// The tests in this file cover a restore Pod that was deleted and created again under the same
// name while a pass of the agent still held the first Pod. Nothing that pass writes may land on
// the second Pod, and the second Pod must then get its own restore.

const replacementContainerID = "replacement-container"

// replacementRestorePod returns the Pod that takes the name of the one restorePod returns.
func replacementRestorePod(annotations map[string]string) *corev1.Pod {
	replacement := restorePod(annotations)
	replacement.UID = ktypes.UID("replacement-pod-uid")
	replacement.Status.ContainerStatuses[0].ContainerID = "containerd://" + replacementContainerID
	return replacement
}

// replacedPodController returns a controller whose API server holds live, with a Ready snapshot to
// restore from, and the restores the controller runs. The API server answers a Pod write that
// names a UID as kube-apiserver does: 404 when the Pod is gone, and 422 when the Pod of that name
// has another UID. The fake clientset alone would accept the write.
func replacedPodController(t *testing.T, live *corev1.Pod) (*NodeController, *[]executor.RestoreRequest) {
	t.Helper()
	snapshot, content := readySnapshotObjects()
	w := makeTestController(t, live, snapshot, content)
	path, err := nsmount.ResolveArtifactPath(w.config.Storage.BasePath, string(content.UID), "main")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(path, 0o700))

	restores := &[]executor.RestoreRequest{}
	w.restoreFn = func(_ context.Context, _ snapshotruntime.Runtime, _ logr.Logger, request executor.RestoreRequest, _ executor.RestoreMounter) (executor.RestoreResult, error) {
		*restores = append(*restores, request)
		return executor.RestoreResult{PlaceholderHostPID: 4242, RestoredPID: 1}, nil
	}

	clientset := w.clientset.(*fake.Clientset)
	clientset.PrependReactor("patch", "pods", func(action clientgotesting.Action) (bool, runtime.Object, error) {
		patchAction := action.(clientgotesting.PatchAction)
		var patch struct {
			Metadata struct {
				UID ktypes.UID `json:"uid"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(patchAction.GetPatch(), &patch); err != nil {
			return true, nil, err
		}
		if patch.Metadata.UID == "" {
			return false, nil, nil
		}
		stored, err := clientset.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), patchAction.GetNamespace(), patchAction.GetName())
		if err != nil {
			return true, nil, err
		}
		if stored.(*corev1.Pod).UID != patch.Metadata.UID {
			return true, nil, apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, patchAction.GetName(), field.ErrorList{
				field.Invalid(field.NewPath("metadata", "uid"), string(patch.Metadata.UID), "field is immutable"),
			})
		}
		return false, nil, nil
	})
	return w, restores
}

func liveRestorePod(t *testing.T, w *NodeController, pod *corev1.Pod) *corev1.Pod {
	t.Helper()
	live, err := w.clientset.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	return live
}

// requireRefusedOnce asserts that the refused write cost one warning event, on the Pod that is gone.
func requireRefusedOnce(t *testing.T, w *NodeController, reason string, gone *corev1.Pod) {
	t.Helper()
	refused := eventsForReason(w.clientset.(*fake.Clientset), reason)
	require.Len(t, refused, 1)
	assert.Equal(t, gone.UID, refused[0].InvolvedObject.UID)
	assert.Contains(t, refused[0].Message, "metadata.uid")
}

// requireRestoredOnce asserts that the replacement got one restore, into its own container.
func requireRestoredOnce(t *testing.T, w *NodeController, restores *[]executor.RestoreRequest, replacement *corev1.Pod) *corev1.Pod {
	t.Helper()
	require.Len(t, *restores, 1, "the replacement must get its own restore")
	assert.Equal(t, replacementContainerID, (*restores)[0].ContainerID)
	live := liveRestorePod(t, w, replacement)
	condition := restoredPodCondition(live)
	require.NotNil(t, condition)
	assert.Equal(t, corev1.ConditionTrue, condition.Status)
	assert.Empty(t, eventsForReason(w.clientset.(*fake.Clientset), restoreAlreadyFailedReason))
	return live
}

// The agent's cache is behind: it still holds a Pod that was evicted, and the API server holds the
// replacement. The failure of the evicted Pod must not land on the replacement: a Pod that carries
// it reads as a restore that already ended, and the agent never restores it.
func TestRestoreStatusOfAReplacedPodDoesNotReachItsReplacement(t *testing.T) {
	annotations := map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"}
	evicted := restorePod(annotations)
	evicted.Status.Phase = corev1.PodFailed
	replacement := replacementRestorePod(annotations)
	w, restores := replacedPodController(t, replacement)

	processQueuedRestorePod(t, w, evicted)

	live := liveRestorePod(t, w, replacement)
	assert.Nil(t, restoredPodCondition(live), "the replacement must not get the outcome of the Pod before it")
	requireRefusedOnce(t, w, restoreStatusUpdateFailedReason, evicted)

	// The cache then delivers the replacement as the API server has it.
	processQueuedRestorePod(t, w, live)

	requireRestoredOnce(t, w, restores, replacement)
}

// The agent's cache is exact, and the Pod is replaced while its preflight runs. The preflight
// passes, so the pass goes on to protect the Pod it holds. That write replaces the finalizer list:
// on the replacement it would add the agent's finalizer and drop the replacement's own.
func TestRestoreFinalizerOfAReplacedPodDoesNotReachItsReplacement(t *testing.T) {
	annotations := map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"}
	first := restorePod(annotations)
	replacement := replacementRestorePod(annotations)
	replacement.Finalizers = []string{"example.com/other"}
	w, restores := replacedPodController(t, first)
	replaced := false
	w.client = interceptor.NewClient(w.client.(client.WithWatch), interceptor.Funcs{
		// The first read of the preflight, for the PodSnapshot.
		Get: func(ctx context.Context, delegated client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if !replaced {
				replaced = true
				pods := w.clientset.CoreV1().Pods(first.Namespace)
				require.NoError(t, pods.Delete(ctx, first.Name, metav1.DeleteOptions{}))
				_, err := pods.Create(ctx, replacement.DeepCopy(), metav1.CreateOptions{})
				require.NoError(t, err)
			}
			return delegated.Get(ctx, key, obj, opts...)
		},
	})

	processQueuedRestorePod(t, w, first)

	live := liveRestorePod(t, w, replacement)
	require.Equal(t, replacement.UID, live.UID)
	assert.Equal(t, []string{"example.com/other"}, live.Finalizers, "the replacement must keep its finalizers")
	assert.Nil(t, restoredPodCondition(live))
	assert.Empty(t, *restores, "the pass of the Pod that is gone must not start a restore")
	requireRefusedOnce(t, w, restoreFinalizerUpdateFailedReason, first)

	processQueuedRestorePod(t, w, live)

	live = requireRestoredOnce(t, w, restores, replacement)
	assert.Equal(t, []string{"example.com/other"}, live.Finalizers, "the replacement's restore must leave its other finalizer")
}

// The agent's cache is behind: it still holds a Pod whose restore ended, with the agent's
// finalizer on it. The replacement is being restored and carries the finalizer of that restore,
// and one of another controller. Releasing the Pod that is gone must not remove either.
func TestFinishedRestoreOfAReplacedPodKeepsTheFinalizersOfItsReplacement(t *testing.T) {
	annotations := map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"}
	finished := restorePod(annotations)
	finished.Finalizers = []string{restorePodFinalizer}
	finished.Status.Conditions = append(finished.Status.Conditions, corev1.PodCondition{
		Type:   corev1.PodConditionType(podcontract.RestoredCondition),
		Status: corev1.ConditionFalse,
		Reason: podcontract.RestoreReasonFailed,
	})
	replacement := replacementRestorePod(annotations)
	replacement.Finalizers = []string{"example.com/other", restorePodFinalizer}
	w, _ := replacedPodController(t, replacement)

	processQueuedRestorePod(t, w, finished)

	live := liveRestorePod(t, w, replacement)
	assert.Equal(t, []string{"example.com/other", restorePodFinalizer}, live.Finalizers, "the replacement must keep its finalizers")
	requireRefusedOnce(t, w, restoreFinalizerUpdateFailedReason, finished)
}

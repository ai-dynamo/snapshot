// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// The tests in this file cover a restore Pod that carries the finalizer of another controller. A
// pass holds one copy of the Pod from its start to the end of the restore, so its two finalizer
// writes must name the agent's own entry and leave the rest of the list as the API server has it.

// The finalizer every Pod of a Job carries until the Job controller has counted it.
const jobTrackingFinalizer = "batch.kubernetes.io/job-tracking"

// patchFinalizerAsItsOwner writes one finalizer as kube-controller-manager does for a Pod: key is
// "finalizers" to add it and "$deleteFromPrimitiveList/finalizers" to remove it.
func patchFinalizerAsItsOwner(t *testing.T, w *NodeController, pod *corev1.Pod, key, finalizer string) {
	t.Helper()
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{key: []string{finalizer}}})
	require.NoError(t, err)
	_, err = w.clientset.CoreV1().Pods(pod.Namespace).Patch(context.Background(), pod.Name, ktypes.StrategicMergePatchType, patch, metav1.PatchOptions{})
	require.NoError(t, err)
}

// restoreDuringWhich makes the restore of w succeed after during has run.
func restoreDuringWhich(w *NodeController, during func()) {
	w.restoreFn = func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (executor.RestoreResult, error) {
		during()
		return executor.RestoreResult{PlaceholderHostPID: 4242, RestoredPID: 1}, nil
	}
}

func TestRestoreKeepsAFinalizerAddedDuringTheRestore(t *testing.T) {
	pod := restorePod(map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"})
	w, _ := replacedPodController(t, pod)
	restoreDuringWhich(w, func() { patchFinalizerAsItsOwner(t, w, pod, "finalizers", "example.com/other") })

	processQueuedRestorePod(t, w, pod)

	live := liveRestorePod(t, w, pod)
	require.NotNil(t, restoredPodCondition(live))
	assert.Equal(t, []string{"example.com/other"}, live.Finalizers, "the agent removes its own finalizer only")
}

// The Job controller removes its finalizer from a Pod that ended. When the pass of that Pod ends
// later, the finalizer must not come back: the Job controller would count the Pod a second time.
func TestRestoreDoesNotBringBackAFinalizerRemovedDuringTheRestore(t *testing.T) {
	pod := restorePod(map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"})
	pod.Finalizers = []string{jobTrackingFinalizer}
	w, _ := replacedPodController(t, pod)
	restoreDuringWhich(w, func() {
		patchFinalizerAsItsOwner(t, w, pod, "$deleteFromPrimitiveList/finalizers", jobTrackingFinalizer)
	})

	processQueuedRestorePod(t, w, pod)

	live := liveRestorePod(t, w, pod)
	require.NotNil(t, restoredPodCondition(live))
	assert.Empty(t, live.Finalizers, "a finalizer that its owner removed must not come back")
}

// The agent's cache is behind by one write: another controller added its finalizer after the copy
// the pass starts from.
func TestRestoreKeepsAFinalizerItsCopyDoesNotHave(t *testing.T) {
	stale := restorePod(map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"})
	current := stale.DeepCopy()
	current.Finalizers = []string{"example.com/other"}
	w, _ := replacedPodController(t, current)
	var during []string
	restoreDuringWhich(w, func() { during = liveRestorePod(t, w, current).Finalizers })

	processQueuedRestorePod(t, w, stale)

	assert.ElementsMatch(t, []string{"example.com/other", restorePodFinalizer}, during, "the agent adds its finalizer to the list")
	assert.Equal(t, []string{"example.com/other"}, liveRestorePod(t, w, current).Finalizers, "the agent removes its own finalizer only")
}

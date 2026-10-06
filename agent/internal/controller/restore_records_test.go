// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

func TestRecordRestoredContainerIDCommitsCopyOnlyAfterPatch(t *testing.T) {
	for _, failPatch := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[failPatch], func(t *testing.T) {
			pod := multiRestorePod()
			setRestoredContainerIDs(t, pod, map[string]string{"engine-0": "engine-0-id"})
			before := pod.DeepCopy()
			// Another actor's annotation exists only in live state. The patch
			// must not replace metadata or erase fields absent from our copy.
			live := pod.DeepCopy()
			live.Annotations["example.com/concurrent"] = "keep"
			w := makeTestController(t, live)
			w.clientset.(*fake.Clientset).PrependReactor("patch", "pods", func(action clientgotesting.Action) (bool, runtime.Object, error) {
				patch := action.(clientgotesting.PatchAction)
				assert.Equal(t, before, pod, "local state must remain unchanged until the API write succeeds")
				var payload map[string]any
				require.NoError(t, json.Unmarshal(patch.GetPatch(), &payload))
				assert.Equal(t, map[string]any{"metadata": map[string]any{"annotations": map[string]any{
					podcontract.RestoredContainerIDsAnnotation: `{"engine-0":"engine-0-id","engine-1":"replacement"}`,
				}}}, payload)
				if failPatch {
					return true, nil, errors.New("annotation patch failed")
				}
				return false, nil, nil
			})
			err := w.recordRestoredContainerID(context.Background(), pod, "engine-1", "replacement")
			if failPatch {
				require.ErrorContains(t, err, "annotation patch failed")
				assert.Equal(t, before, pod)
				assert.Empty(t, liveRestoredContainerID(t, w, pod, "engine-1"))
			} else {
				require.NoError(t, err)
				ids, err := restoredContainerIDs(pod)
				require.NoError(t, err)
				assert.Equal(t, map[string]string{"engine-0": "engine-0-id", "engine-1": "replacement"}, ids)
				assert.Equal(t, "replacement", liveRestoredContainerID(t, w, pod, "engine-1"))
			}
			got, err := w.clientset.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, "keep", got.Annotations["example.com/concurrent"])
		})
	}
}

func TestRestoredContainerIDsAcceptsLegacyAbsence(t *testing.T) {
	for _, tc := range []struct {
		name        string
		annotations map[string]string
	}{
		{"nil annotations", nil},
		{"absent", map[string]string{"example.com/unrelated": "keep"}},
		{"empty", map[string]string{podcontract.RestoredContainerIDsAnnotation: ""}},
		{"empty object", map[string]string{podcontract.RestoredContainerIDsAnnotation: "{}"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: tc.annotations}}
			before := pod.DeepCopy()
			ids, err := restoredContainerIDs(pod)
			require.NoError(t, err)
			require.NotNil(t, ids)
			assert.Empty(t, ids)
			assert.Equal(t, before, pod)
		})
	}
}

func TestLegacyRestorePodsWithoutRecords(t *testing.T) {
	for _, value := range []string{"absent", "", "{}"} {
		for _, succeeded := range []bool{false, true} {
			name := value + "/initial"
			if succeeded {
				name = value + "/succeeded"
			}
			t.Run(name, func(t *testing.T) {
				pod := restorePod(map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"})
				if value != "absent" {
					pod.Annotations[podcontract.RestoredContainerIDsAnnotation] = value
				}
				if succeeded {
					setPodCondition(&pod.Status, corev1.PodCondition{
						Type:   corev1.PodConditionType(podcontract.RestoredCondition),
						Status: corev1.ConditionTrue, Reason: podcontract.RestoreReasonSucceeded,
					})
					// A restarted legacy destination still lacks evidence about
					// which incarnation was restored. Do not infer permission.
					pod.Status.ContainerStatuses[0].ContainerID = "containerd://replacement"
				}
				w := makeTestController(t, pod)
				calls := 0
				w.restoreFn = func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (int, error) {
					calls++
					return 4242, nil
				}
				if succeeded {
					processQueuedRestorePod(t, w, pod)
					assert.Zero(t, calls)
					assert.False(t, hasPodStatusApply(w))
					assert.Empty(t, liveRestoredContainerID(t, w, pod, "main"))
				} else {
					plan := &restorePlan{artifact: &restoreArtifact{}, mappings: []podcontract.ContainerMapping{{Source: "main", Destination: "main"}}}
					assert.False(t, w.restorePodContainers(context.Background(), pod, plan, "inference/restore-worker"))
					assert.Equal(t, 1, calls)
					assert.Equal(t, testContainerID, liveRestoredContainerID(t, w, pod, "main"))
				}
				live, err := w.clientset.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
				require.NoError(t, err)
				assert.True(t, isRestoreSucceeded(live))
			})
		}
	}
}

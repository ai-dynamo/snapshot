// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// TestRestoreStatusOfAReplacedPodDoesNotReachItsReplacement covers a restore Pod that was deleted
// and created again under the same name while the agent still held the first one from its cache.
// The outcome of the first Pod must not land on the second: a Pod that carries it reads as a
// restore that already ended, and the agent never restores it.
func TestRestoreStatusOfAReplacedPodDoesNotReachItsReplacement(t *testing.T) {
	annotations := map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"}
	evicted := restorePod(annotations)
	evicted.Status.Phase = corev1.PodFailed
	replacement := restorePod(annotations)
	replacement.UID = ktypes.UID("replacement-pod-uid")

	// The API server holds the replacement, and refuses a write that names another UID.
	w := makeTestController(t, replacement)
	clientset := w.clientset.(*fake.Clientset)
	clientset.PrependReactor("patch", "pods", func(action clientgotesting.Action) (bool, runtime.Object, error) {
		var patch struct {
			Metadata struct {
				UID ktypes.UID `json:"uid"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(action.(clientgotesting.PatchAction).GetPatch(), &patch); err != nil {
			return true, nil, err
		}
		if patch.Metadata.UID != "" && patch.Metadata.UID != replacement.UID {
			return true, nil, errors.New(`Pod "restore-worker" is invalid: metadata.uid: field is immutable`)
		}
		return false, nil, nil
	})

	// The agent's cache still holds the evicted Pod.
	processQueuedRestorePod(t, w, evicted)

	live, err := clientset.CoreV1().Pods(replacement.Namespace).Get(context.Background(), replacement.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Nil(t, restoredPodCondition(live), "the replacement must not get the outcome of the Pod before it")

	// The cache then delivers the replacement as the API server has it.
	processQueuedRestorePod(t, w, live)

	assert.False(t, w.restoreHandled(live), "the replacement must stay open for its own restore")
	assert.Empty(t, eventsForReason(clientset, restoreAlreadyFailedReason))
}

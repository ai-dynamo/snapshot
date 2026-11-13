// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package targetstatus

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

func targetPod(conditions ...corev1.PodCondition) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "inference", UID: "pod-uid", ResourceVersion: "41"},
		Status:     corev1.PodStatus{Conditions: conditions},
	}
}

func TestConfigurationIsFencedOnTheReadPod(t *testing.T) {
	cfg := configuration(targetPod(), RestoreFieldManager, []corev1.PodCondition{
		{Type: podcontract.RestoredCondition, Status: corev1.ConditionFalse, Reason: "GroupPending", Message: "waiting"},
	})
	assert.Equal(t, "pod-uid", string(*cfg.UID))
	assert.Equal(t, "41", *cfg.ResourceVersion)
	require.Len(t, cfg.Status.Conditions, 1)
	assert.Equal(t, "GroupPending", *cfg.Status.Conditions[0].Reason)
	assert.False(t, cfg.Status.Conditions[0].LastTransitionTime.IsZero())
}

func TestConfigurationKeepsTheTransitionTimeOfAnUnchangedStatus(t *testing.T) {
	then := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	pod := targetPod(corev1.PodCondition{Type: podcontract.RestoredCondition, Status: corev1.ConditionFalse,
		Reason: "GroupPending", LastTransitionTime: then})

	same := configuration(pod, RestoreFieldManager, []corev1.PodCondition{
		{Type: podcontract.RestoredCondition, Status: corev1.ConditionFalse, Reason: podcontract.RestoreReasonInProgress},
	})
	assert.True(t, then.Equal(same.Status.Conditions[0].LastTransitionTime), "a new reason keeps the status's time")

	changed := configuration(pod, RestoreFieldManager, []corev1.PodCondition{
		{Type: podcontract.RestoredCondition, Status: corev1.ConditionTrue, Reason: podcontract.RestoreReasonSucceeded},
	})
	assert.True(t, changed.Status.Conditions[0].LastTransitionTime.After(then.Time), "a new status moves the time")
}

func TestConfigurationWritesAnExistingRecordBackUnchanged(t *testing.T) {
	then := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	recorded := corev1.PodCondition{Type: podcontract.RestoreStartedCondition, Status: corev1.ConditionTrue,
		Reason: "RestoreStarted", Message: "first", LastTransitionTime: then}
	cfg := configuration(targetPod(recorded), StartedFieldManager, []corev1.PodCondition{
		{Type: podcontract.RestoreStartedCondition, Status: corev1.ConditionTrue, Reason: "RestoreStarted", Message: "second"},
	})
	assert.Equal(t, "first", *cfg.Status.Conditions[0].Message)
	assert.True(t, then.Equal(cfg.Status.Conditions[0].LastTransitionTime))
}

func TestConfigurationCarriesTheGroupsReasonForward(t *testing.T) {
	groupFailed := corev1.PodCondition{Type: podcontract.GroupRestoreFailedCondition, Status: corev1.ConditionTrue,
		Reason: "PodIPChanged", LastTransitionTime: metav1.Now()}
	restored := corev1.PodCondition{Type: podcontract.RestoredCondition, Status: corev1.ConditionFalse, Reason: podcontract.RestoreReasonFailed}

	cfg := configuration(targetPod(groupFailed), RestoreFieldManager, []corev1.PodCondition{restored})
	require.Len(t, cfg.Status.Conditions, 2)
	assert.Equal(t, "PodIPChanged", *cfg.Status.Conditions[1].Reason)

	cfg = configuration(targetPod(groupFailed), StartedFieldManager, []corev1.PodCondition{
		{Type: podcontract.RestoreStartedCondition, Status: corev1.ConditionTrue},
	})
	assert.Len(t, cfg.Status.Conditions, 1, "only the restore field manager owns the group's reason")
}

func TestApplyRefusesToWithdrawTheRecord(t *testing.T) {
	client := fake.NewClientset()
	pod := targetPod()
	_, err := Apply(context.Background(), client, pod, pod.UID, StartedFieldManager,
		corev1.PodCondition{Type: podcontract.RestoreStartedCondition, Status: corev1.ConditionUnknown})
	require.ErrorContains(t, err, "never set to Unknown")
	_, err = Apply(context.Background(), client, pod, pod.UID, RestoreFieldManager,
		corev1.PodCondition{Type: podcontract.RestoreStartedCondition, Status: corev1.ConditionTrue})
	require.ErrorContains(t, err, "written only by "+StartedFieldManager)
	assert.Empty(t, client.Actions(), "a refused write reaches no API server")
}

func TestApplyRefusesAnotherPod(t *testing.T) {
	client := fake.NewClientset()
	pod := targetPod()
	_, err := Apply(context.Background(), client, pod, "pinned-uid", RestoreFieldManager,
		corev1.PodCondition{Type: podcontract.RestoredCondition, Status: corev1.ConditionFalse})
	require.ErrorIs(t, err, ErrReplaced)
	assert.Empty(t, client.Actions())
}

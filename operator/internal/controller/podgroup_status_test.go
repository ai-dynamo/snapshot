// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// The PodGroupSnapshot tests cover the shared rules through its derive. These
// cover the parts that only a PodGroupRestore exercises.

// failedView is a member that failed with reason, after starting if started.
func failedView(id string, rank int32, started bool, reason string) podGroupMemberView {
	return podGroupMemberView{id: id, rank: rank, started: started, hasWork: true, failed: true,
		reason: reason, message: reason + " message"}
}

func TestPodGroupAttemptPassesRestoreReasons(t *testing.T) {
	running := podGroupMemberView{id: "b", rank: 1, started: true, hasWork: true}
	for _, tc := range []struct {
		name    string
		members []podGroupMemberView
		want    string
	}{
		{"committed: PodIPChanged",
			[]podGroupMemberView{failedView("a", 0, true, snapshotv1alpha1.PodGroupReasonPodIPChanged), running},
			snapshotv1alpha1.PodGroupReasonPodIPChanged},
		{"committed: GateBudgetTooShort is MemberFailed",
			[]podGroupMemberView{failedView("a", 0, false, snapshotv1alpha1.PodGroupReasonGateBudgetTooShort), running},
			snapshotv1alpha1.PodGroupReasonMemberFailed},
		{"not committed: GateBudgetTooShort",
			[]podGroupMemberView{
				failedView("a", 0, false, snapshotv1alpha1.PodGroupReasonGateBudgetTooShort),
				failedView("b", 1, false, snapshotv1alpha1.PodGroupReasonRefused),
			},
			snapshotv1alpha1.PodGroupReasonGateBudgetTooShort},
		{"not committed: PodIPChanged",
			[]podGroupMemberView{
				failedView("a", 0, false, snapshotv1alpha1.PodGroupReasonRefused),
				failedView("b", 1, false, snapshotv1alpha1.PodGroupReasonPodIPChanged),
			},
			snapshotv1alpha1.PodGroupReasonPodIPChanged},
		{"not committed: any other reason is Refused",
			[]podGroupMemberView{
				failedView("a", 0, false, snapshotv1alpha1.PodGroupReasonRefused),
				failedView("b", 1, false, "RestoreIncompatible"),
			},
			snapshotv1alpha1.PodGroupReasonRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var conditions []metav1.Condition
			newPodGroupAttempt(&podGroupSnapshotKind, 600, podGroupAdmission{}, false, false, tc.members).
				updateConditions(&conditions, pgNow)

			failed := meta.FindStatusCondition(conditions, snapshotv1alpha1.PodGroupConditionFailed)
			require.NotNil(t, failed)
			assert.Equal(t, tc.want, failed.Reason)
		})
	}
}

func TestPodGroupAttemptAdmittedTakesTheFailure(t *testing.T) {
	members := []podGroupMemberView{failedView("a", 0, false, "Own"), failedView("b", 1, false, "Own")}

	t.Run("the failure it just decided", func(t *testing.T) {
		var conditions []metav1.Condition
		newPodGroupAttempt(&podGroupSnapshotKind, 600, podGroupAdmission{}, false, false, members).
			updateConditions(&conditions, pgNow)

		admitted := meta.FindStatusCondition(conditions, snapshotv1alpha1.PodGroupConditionAdmitted)
		require.NotNil(t, admitted)
		assert.Equal(t, metav1.ConditionFalse, admitted.Status)
		assert.Equal(t, snapshotv1alpha1.PodGroupReasonRefused, admitted.Reason)
		assert.Equal(t, `member "a" (rank 0) failed with Own: Own message`, admitted.Message)
	})

	t.Run("the recorded failure, never decided again", func(t *testing.T) {
		conditions := []metav1.Condition{{Type: snapshotv1alpha1.PodGroupConditionFailed, Status: metav1.ConditionTrue,
			Reason: snapshotv1alpha1.PodGroupReasonDeadlineExceeded, Message: "the first cause"}}
		newPodGroupAttempt(&podGroupSnapshotKind, 600, podGroupAdmission{}, false, false, members).
			updateConditions(&conditions, pgNow)

		require.Len(t, conditions, 2)
		assert.Equal(t, "the first cause", conditions[0].Message)
		assert.Equal(t, snapshotv1alpha1.PodGroupReasonDeadlineExceeded, conditions[1].Reason)
		assert.Equal(t, "the first cause", conditions[1].Message)
	})
}

func TestPodGroupAttemptDeadlineWhileATargetPodIsPending(t *testing.T) {
	conditions := []metav1.Condition{{Type: snapshotv1alpha1.PodGroupConditionAdmitted, Status: metav1.ConditionFalse,
		Reason: snapshotv1alpha1.PodGroupReasonTargetPodPending, Message: `target "a": Pod "pod-a" does not exist yet`}}
	newPodGroupAttempt(&podGroupSnapshotKind, 600, podGroupAdmission{}, false, true, []podGroupMemberView{{id: "a"}}).
		updateConditions(&conditions, pgNow)

	failed := meta.FindStatusCondition(conditions, snapshotv1alpha1.PodGroupConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, `the deadline of 600 seconds passed while admission was waiting: target "a": Pod "pod-a" does not exist yet`,
		failed.Message)
}

func TestPodGroupTransitionsReleased(t *testing.T) {
	released := metav1.Condition{Type: snapshotv1alpha1.PodGroupConditionReleased, Status: metav1.ConditionTrue,
		Reason: "Released", Message: "Released message"}
	assert.Equal(t, []podGroupEvent{{corev1.EventTypeNormal, "Released", "Released message"}},
		podGroupTransitions(nil, []metav1.Condition{released}))
	assert.Empty(t, podGroupTransitions([]metav1.Condition{released}, []metav1.Condition{released}))
}

func TestPodGroupRequeuePollsOnlyForUnwatchedWaits(t *testing.T) {
	unfinished := []podGroupMemberView{{id: "a"}}
	deadline := pgNow.Add(time.Hour)
	for _, tc := range []struct {
		reason string
		want   time.Duration
	}{
		{snapshotv1alpha1.PodGroupReasonAgentNotReady, podGroupAdmissionRequeue},
		{snapshotv1alpha1.PodGroupReasonSourcePodUnscheduled, podGroupAdmissionRequeue},
		{snapshotv1alpha1.PodGroupReasonPreviousGroupCleanup, time.Hour},
		{snapshotv1alpha1.PodGroupReasonTargetPodPending, time.Hour},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			admission := podGroupAdmission{podGroupAdmissionWaiting, tc.reason, ""}
			assert.Equal(t, tc.want, podGroupRequeue(admission, deadline, pgNow, unfinished))
		})
	}
}

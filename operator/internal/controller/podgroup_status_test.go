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

func TestPodGroupAttemptDeadlineWhileATargetPodWaits(t *testing.T) {
	for reason, wait := range map[string]string{
		snapshotv1alpha1.PodGroupReasonTargetPodPending:     `target "a": Pod "pod-a" does not exist yet`,
		snapshotv1alpha1.PodGroupReasonTargetPodUnscheduled: `target "a": Pod "pod-a" is not scheduled to a node yet`,
	} {
		t.Run(reason, func(t *testing.T) {
			conditions := []metav1.Condition{{Type: snapshotv1alpha1.PodGroupConditionAdmitted, Status: metav1.ConditionFalse,
				Reason: reason, Message: wait}}
			newPodGroupAttempt(&podGroupSnapshotKind, 600, podGroupAdmission{}, false, true, []podGroupMemberView{{id: "a"}}).
				updateConditions(&conditions, pgNow)

			failed := meta.FindStatusCondition(conditions, snapshotv1alpha1.PodGroupConditionFailed)
			require.NotNil(t, failed)
			assert.Equal(t, "the deadline of 600 seconds passed while admission was waiting: "+wait, failed.Message)
		})
	}
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
		{snapshotv1alpha1.PodGroupReasonTargetPodUnscheduled, time.Hour},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			admission := podGroupAdmission{podGroupAdmissionWaiting, tc.reason, ""}
			assert.Equal(t, tc.want, podGroupRequeue(admission, deadline, pgNow, unfinished))
		})
	}
}

func TestIsIrreversiblePodGroupChange(t *testing.T) {
	admitted := podGroupMemberView{id: "a", hasWork: true}
	running := podGroupMemberView{id: "a", hasWork: true, started: true}
	done := podGroupMemberView{id: "a", hasWork: true, started: true, done: true}
	failed := failedView("a", 0, true, "CriuDumpFailed")
	cond := func(condType string, status metav1.ConditionStatus, reason string) metav1.Condition {
		return metav1.Condition{Type: condType, Status: status, Reason: reason}
	}
	failedCond := cond(snapshotv1alpha1.PodGroupConditionFailed, metav1.ConditionTrue, snapshotv1alpha1.PodGroupReasonMemberFailed)
	readyCond := cond(snapshotv1alpha1.PodGroupConditionReady, metav1.ConditionTrue, "Checkpointed")
	withdrawn := cond(snapshotv1alpha1.PodGroupConditionReady, metav1.ConditionFalse, snapshotv1alpha1.PodGroupReasonArtifactUnavailable)
	committed := cond(snapshotv1alpha1.PodGroupConditionAdmitted, metav1.ConditionTrue, snapshotv1alpha1.PodGroupReasonCommitted)

	for _, tc := range []struct {
		name                string
		persisted, derived  []podGroupMemberView
		persistedConditions []metav1.Condition
		derivedConditions   []metav1.Condition
		releases            bool
		want                bool
	}{
		{name: "a member succeeds", persisted: []podGroupMemberView{running}, derived: []podGroupMemberView{done}, want: true},
		{name: "a member fails", persisted: []podGroupMemberView{running}, derived: []podGroupMemberView{failed}, want: true},
		{name: "a member the persisted status lacks is final", derived: []podGroupMemberView{failed}, want: true},
		{
			name: "Failed turns True", persisted: []podGroupMemberView{failed}, derived: []podGroupMemberView{failed},
			derivedConditions: []metav1.Condition{failedCond}, want: true,
		},
		{
			name: "Ready turns True", persisted: []podGroupMemberView{done}, derived: []podGroupMemberView{done},
			persistedConditions: []metav1.Condition{withdrawn}, derivedConditions: []metav1.Condition{readyCond}, want: true,
		},
		{
			name: "the finalizer would be released", persisted: []podGroupMemberView{failed}, derived: []podGroupMemberView{failed},
			persistedConditions: []metav1.Condition{failedCond}, derivedConditions: []metav1.Condition{failedCond},
			releases: true, want: true,
		},
		{name: "nothing changes", persisted: []podGroupMemberView{running}, derived: []podGroupMemberView{running}},
		{name: "a member starts", persisted: []podGroupMemberView{admitted}, derived: []podGroupMemberView{running}},
		{
			name:      "a final member stays final, and Failed stays True",
			persisted: []podGroupMemberView{failed}, derived: []podGroupMemberView{failed},
			persistedConditions: []metav1.Condition{failedCond}, derivedConditions: []metav1.Condition{failedCond},
		},
		{
			name: "Admitted turns True", persisted: []podGroupMemberView{admitted}, derived: []podGroupMemberView{running},
			derivedConditions: []metav1.Condition{committed},
		},
		{
			name: "Ready is withdrawn", persisted: []podGroupMemberView{done}, derived: []podGroupMemberView{done},
			persistedConditions: []metav1.Condition{readyCond}, derivedConditions: []metav1.Condition{withdrawn},
		},
		{
			name: "Ready appears False", persisted: []podGroupMemberView{done}, derived: []podGroupMemberView{done},
			derivedConditions: []metav1.Condition{withdrawn},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isIrreversiblePodGroupChange(tc.persisted, tc.derived,
				tc.persistedConditions, tc.derivedConditions, tc.releases))
		})
	}
}

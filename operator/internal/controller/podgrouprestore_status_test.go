// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// prSpec is a PodGroupRestore spec whose targets a, b, ... name the restore
// Pods pod-a, pod-b, ...
func prSpec(ids ...string) *snapshotv1alpha1.PodGroupRestoreSpec {
	spec := &snapshotv1alpha1.PodGroupRestoreSpec{
		SnapshotRef:     snapshotv1alpha1.ObjectIdentity{Name: "group", UID: "group-uid"},
		DeadlineSeconds: 600,
	}
	for _, id := range ids {
		spec.Targets = append(spec.Targets, snapshotv1alpha1.PodGroupRestoreTarget{
			ID: id, PodRef: snapshotv1alpha1.TargetPodReference{Name: "pod-" + id},
		})
	}
	return spec
}

// prPin is what admission pins for target id.
func prPin(id string) snapshotv1alpha1.PodGroupRestoreMemberStatus {
	return snapshotv1alpha1.PodGroupRestoreMemberStatus{
		ID:          id,
		Pod:         &snapshotv1alpha1.ObjectIdentity{Name: "pod-" + id, UID: types.UID("pod-uid-" + id)},
		NodeName:    "node-" + id,
		Containers:  []snapshotv1alpha1.PodGroupRestoreContainerStatus{{Name: "main", ContainerID: "containerd://" + id}},
		TargetPodIP: "10.0.0.1",
	}
}

// prPinned is a member status entry as an earlier reconcile wrote it.
func prPinned(id string, phase snapshotv1alpha1.RestorePhase) snapshotv1alpha1.PodGroupRestoreMemberStatus {
	if phase == snapshotv1alpha1.RestorePhasePending {
		return snapshotv1alpha1.PodGroupRestoreMemberStatus{ID: id, Phase: phase}
	}
	m := prPin(id)
	m.Phase = phase
	if phase == snapshotv1alpha1.RestorePhaseRestoring || phase == snapshotv1alpha1.RestorePhaseReleased {
		m.StartedAt = pgStartedAt.DeepCopy()
	}
	return m
}

// prFailed is m failed with reason and its "<reason> message".
func prFailed(m snapshotv1alpha1.PodGroupRestoreMemberStatus, reason string) snapshotv1alpha1.PodGroupRestoreMemberStatus {
	m.Phase, m.Reason, m.Message = snapshotv1alpha1.RestorePhaseFailed, reason, reason+" message"
	return m
}

var prStarted = corev1.PodCondition{
	Type: podcontract.RestoreStartedCondition, Status: corev1.ConditionTrue, Reason: "Started", LastTransitionTime: pgStartedAt,
}

func prRestored(status corev1.ConditionStatus, reason string) corev1.PodCondition {
	return corev1.PodCondition{Type: podcontract.RestoredCondition, Status: status, Reason: reason, Message: reason + " message"}
}

func prGroupFailed(reason string) corev1.PodCondition {
	return corev1.PodCondition{
		Type: podcontract.GroupRestoreFailedCondition, Status: corev1.ConditionTrue, Reason: reason, Message: reason + " message",
	}
}

var (
	prReleased     = prRestored(corev1.ConditionTrue, podcontract.RestoreReasonSucceeded)
	prRestoreError = prRestored(corev1.ConditionFalse, podcontract.RestoreReasonFailed)
)

// prPod observes target id's pinned Pod carrying conditions.
func prPod(id string, conditions ...corev1.PodCondition) podGroupRestoreMemberObservation {
	return podGroupRestoreMemberObservation{report: podGroupRestoreReportOf(prReportedPod(id, conditions...))}
}

// prReportedPod is target id's restore Pod carrying conditions, with another
// condition the report does not read.
func prReportedPod(id string, conditions ...corev1.PodCondition) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-" + id, Namespace: "ns", UID: types.UID("pod-uid-" + id)},
		Status: corev1.PodStatus{Conditions: append([]corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			conditions...)},
	}
}

// prObserved builds an observation of the given members with a deadline an
// hour away.
func prObserved(members ...podGroupRestoreMemberObservation) podGroupRestoreObservation {
	return podGroupRestoreObservation{members: members, deadline: pgNow.Add(time.Hour)}
}

// prPinning is an observation in which admission passed for ids.
func prPinning(ids ...string) podGroupRestoreObservation {
	o := prObserved()
	o.admission.state = podGroupAdmissionPassed
	for _, id := range ids {
		o.pins = append(o.pins, prPin(id))
	}
	return o
}

// prStatus is a previous status holding members.
func prStatus(members ...snapshotv1alpha1.PodGroupRestoreMemberStatus) snapshotv1alpha1.PodGroupRestoreStatus {
	return snapshotv1alpha1.PodGroupRestoreStatus{Members: members}
}

func prDerive(spec *snapshotv1alpha1.PodGroupRestoreSpec, observed podGroupRestoreObservation,
	previous snapshotv1alpha1.PodGroupRestoreStatus,
) snapshotv1alpha1.PodGroupRestoreStatus {
	return derivePodGroupRestoreStatus(spec, observed, previous, pgNow)
}

func prCondition(status snapshotv1alpha1.PodGroupRestoreStatus, condType string) *metav1.Condition {
	return meta.FindStatusCondition(status.Conditions, condType)
}

func TestDerivePodGroupRestoreMemberTable(t *testing.T) {
	admitted := ptr.To(prPinned("a", snapshotv1alpha1.RestorePhaseAdmitted))
	restoring := ptr.To(prPinned("a", snapshotv1alpha1.RestorePhaseRestoring))
	refused := podGroupAdmission{podGroupAdmissionRefused, snapshotv1alpha1.PodGroupReasonRefused, "target Pod is not a group target"}

	for _, tc := range []struct {
		name         string
		previous     *snapshotv1alpha1.PodGroupRestoreMemberStatus
		observed     podGroupRestoreObservation
		deleting     bool
		pastDeadline bool
		want         snapshotv1alpha1.PodGroupRestoreMemberStatus
	}{
		{
			name:     "nothing observed is Pending",
			observed: prObserved(),
			want:     prPinned("a", snapshotv1alpha1.RestorePhasePending),
		},
		{
			name:     "admission passed pins the member and admits it",
			observed: prPinning("a"),
			want:     prPinned("a", snapshotv1alpha1.RestorePhaseAdmitted),
		},
		{
			name:     "a pinned member with no report stays Admitted",
			previous: admitted,
			observed: prObserved(prPod("a")),
			want:     prPinned("a", snapshotv1alpha1.RestorePhaseAdmitted),
		},
		{
			name:     "RestoreStarted is Restoring and copies startedAt",
			previous: admitted,
			observed: prObserved(prPod("a", prStarted)),
			want:     prPinned("a", snapshotv1alpha1.RestorePhaseRestoring),
		},
		{
			name:     "Restored True is Released",
			previous: restoring,
			observed: prObserved(prPod("a", prStarted, prReleased)),
			want:     prPinned("a", snapshotv1alpha1.RestorePhaseReleased),
		},
		{
			name:     "Restored False with RestoreFailed fails the member with Restored's reason",
			previous: restoring,
			observed: prObserved(prPod("a", prStarted, prRestoreError)),
			want:     prFailed(prPinned("a", snapshotv1alpha1.RestorePhaseRestoring), podcontract.RestoreReasonFailed),
		},
		{
			name:     "Restored False with RestoreIncompatible fails the member",
			previous: admitted,
			observed: prObserved(prPod("a", prRestored(corev1.ConditionFalse, podcontract.RestoreReasonIncompatible))),
			want:     prFailed(prPinned("a", snapshotv1alpha1.RestorePhaseAdmitted), podcontract.RestoreReasonIncompatible),
		},
		{
			name:     "GroupRestoreFailed gives the failed member the group's reason",
			previous: restoring,
			observed: prObserved(prPod("a", prStarted, prRestoreError, prGroupFailed(snapshotv1alpha1.PodGroupReasonPodIPChanged))),
			want:     prFailed(prPinned("a", snapshotv1alpha1.RestorePhaseRestoring), snapshotv1alpha1.PodGroupReasonPodIPChanged),
		},
		{
			name:     "GroupRestoreFailed while Restored is in progress fails nothing",
			previous: restoring,
			observed: prObserved(prPod("a", prStarted,
				prRestored(corev1.ConditionFalse, podcontract.RestoreReasonInProgress),
				prGroupFailed(snapshotv1alpha1.PodGroupReasonCoordinatorLost))),
			want: prPinned("a", snapshotv1alpha1.RestorePhaseRestoring),
		},
		{
			name:     "GroupRestoreFailed without Restored fails nothing",
			previous: admitted,
			observed: prObserved(prPod("a", prGroupFailed(snapshotv1alpha1.PodGroupReasonGateBudgetTooShort))),
			want:     prPinned("a", snapshotv1alpha1.RestorePhaseAdmitted),
		},
		{
			name:     "an agent's internal wait is not final",
			previous: admitted,
			observed: prObserved(prPod("a", prRestored(corev1.ConditionFalse, "GroupPending"))),
			want:     prPinned("a", snapshotv1alpha1.RestorePhaseAdmitted),
		},
		{
			name:     "a member that fails straight from Admitted keeps its startedAt",
			previous: admitted,
			observed: prObserved(prPod("a", prStarted, prRestoreError)),
			want:     prFailed(prPinned("a", snapshotv1alpha1.RestorePhaseRestoring), podcontract.RestoreReasonFailed),
		},
		{
			name:     "startedAt is copied onto a member that is already Failed",
			previous: ptr.To(prFailed(prPinned("a", snapshotv1alpha1.RestorePhaseAdmitted), podcontract.RestoreReasonFailed)),
			observed: prObserved(prPod("a", prStarted, prRestoreError)),
			want:     prFailed(prPinned("a", snapshotv1alpha1.RestorePhaseRestoring), podcontract.RestoreReasonFailed),
		},
		{
			name:     "a pinned Pod confirmed gone fails the member with TargetPodDeleted",
			previous: restoring,
			observed: prObserved(podGroupRestoreMemberObservation{podDeleted: true}),
			want: func() snapshotv1alpha1.PodGroupRestoreMemberStatus {
				m := prPinned("a", snapshotv1alpha1.RestorePhaseRestoring)
				m.Phase, m.Reason = snapshotv1alpha1.RestorePhaseFailed, snapshotv1alpha1.PodGroupReasonTargetPodDeleted
				m.Message = `Pod "pod-a" was deleted or replaced before the member was released`
				return m
			}(),
		},
		{
			name:         "TargetPodDeleted outranks the deadline",
			previous:     admitted,
			observed:     prObserved(podGroupRestoreMemberObservation{podDeleted: true}),
			pastDeadline: true,
			want: func() snapshotv1alpha1.PodGroupRestoreMemberStatus {
				m := prPinned("a", snapshotv1alpha1.RestorePhaseAdmitted)
				m.Phase, m.Reason = snapshotv1alpha1.RestorePhaseFailed, snapshotv1alpha1.PodGroupReasonTargetPodDeleted
				m.Message = `Pod "pod-a" was deleted or replaced before the member was released`
				return m
			}(),
		},
		{
			name:         "the deadline fails an unfinished member",
			previous:     restoring,
			observed:     prObserved(prPod("a", prStarted)),
			pastDeadline: true,
			want: func() snapshotv1alpha1.PodGroupRestoreMemberStatus {
				m := prPinned("a", snapshotv1alpha1.RestorePhaseRestoring)
				m.Phase, m.Reason = snapshotv1alpha1.RestorePhaseFailed, snapshotv1alpha1.PodGroupReasonDeadlineExceeded
				m.Message = podGroupRestoreDeadlineMemberMessage
				return m
			}(),
		},
		{
			name:         "the deadline leaves a member released in the same pass Released",
			previous:     restoring,
			observed:     prObserved(prPod("a", prStarted, prReleased)),
			pastDeadline: true,
			want:         prPinned("a", snapshotv1alpha1.RestorePhaseReleased),
		},
		{
			name:         "a reported failure outranks the deadline",
			previous:     restoring,
			observed:     prObserved(prPod("a", prStarted, prRestoreError)),
			pastDeadline: true,
			want:         prFailed(prPinned("a", snapshotv1alpha1.RestorePhaseRestoring), podcontract.RestoreReasonFailed),
		},
		{
			name:     "deletion before any member started cancels the member",
			previous: admitted,
			observed: prObserved(prPod("a")),
			deleting: true,
			want: func() snapshotv1alpha1.PodGroupRestoreMemberStatus {
				m := prPinned("a", snapshotv1alpha1.RestorePhaseAdmitted)
				m.Phase, m.Reason = snapshotv1alpha1.RestorePhaseFailed, snapshotv1alpha1.PodGroupReasonCancelled
				m.Message = podGroupRestoreCancelledMemberMessage
				return m
			}(),
		},
		{
			name:     "deletion after the member started leaves it Restoring",
			previous: admitted,
			observed: prObserved(prPod("a", prStarted)),
			deleting: true,
			want:     prPinned("a", snapshotv1alpha1.RestorePhaseRestoring),
		},
		{
			name:     "a refused restore fails the member with the refusal's reason",
			observed: func() podGroupRestoreObservation { o := prObserved(); o.admission = refused; return o }(),
			want: snapshotv1alpha1.PodGroupRestoreMemberStatus{
				Phase: snapshotv1alpha1.RestorePhaseFailed, Reason: snapshotv1alpha1.PodGroupReasonRefused,
				Message: podGroupRestoreRefusedMemberMessage,
			},
		},
		{
			name:     "Failed is final",
			previous: ptr.To(prFailed(prPinned("a", snapshotv1alpha1.RestorePhaseRestoring), podcontract.RestoreReasonFailed)),
			observed: prObserved(prPod("a", prStarted, prReleased)),
			want:     prFailed(prPinned("a", snapshotv1alpha1.RestorePhaseRestoring), podcontract.RestoreReasonFailed),
		},
		{
			name:         "Released is final",
			previous:     ptr.To(prPinned("a", snapshotv1alpha1.RestorePhaseReleased)),
			observed:     prObserved(prPod("a", prStarted, prRestoreError)),
			pastDeadline: true,
			want:         prPinned("a", snapshotv1alpha1.RestorePhaseReleased),
		},
		{
			name:     "a Released member whose Pod is deleted stays Released",
			previous: ptr.To(prPinned("a", snapshotv1alpha1.RestorePhaseReleased)),
			observed: prObserved(podGroupRestoreMemberObservation{podDeleted: true}),
			want:     prPinned("a", snapshotv1alpha1.RestorePhaseReleased),
		},
		{
			name:     "startedAt is written once",
			previous: restoring,
			observed: prObserved(prPod("a", corev1.PodCondition{
				Type: podcontract.RestoreStartedCondition, Status: corev1.ConditionTrue,
				LastTransitionTime: metav1.NewTime(pgNow.Add(-time.Second)),
			})),
			want: prPinned("a", snapshotv1alpha1.RestorePhaseRestoring),
		},
		{
			name:     "a phase never moves back when its Pod is not seen",
			previous: restoring,
			observed: prObserved(),
			want:     prPinned("a", snapshotv1alpha1.RestorePhaseRestoring),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observed := tc.observed
			observed.deleting = tc.deleting
			if tc.pastDeadline {
				observed.deadline = pgNow
			}
			var previous snapshotv1alpha1.PodGroupRestoreStatus
			if tc.previous != nil {
				previous = prStatus(*tc.previous)
			}
			want := tc.want
			want.ID = "a"

			got := prDerive(prSpec("a"), observed, previous)

			require.Len(t, got.Members, 1)
			assert.Equal(t, want, got.Members[0])
		})
	}
}

func TestDerivePodGroupRestoreCapsTheMemberMessage(t *testing.T) {
	// One byte, then two-byte characters, so the cut splits a character.
	long := "x" + strings.Repeat("é", maxMemberMessageLength)
	failure := prRestoreError
	failure.Message = long

	got := prDerive(prSpec("a"), prObserved(prPod("a", prStarted, failure)),
		prStatus(prPinned("a", snapshotv1alpha1.RestorePhaseAdmitted)))

	assert.Equal(t, long[:maxMemberMessageLength-1], got.Members[0].Message)
	failed := prCondition(got, snapshotv1alpha1.PodGroupConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, `member "a" (rank 0) failed with RestoreFailed: `+got.Members[0].Message, failed.Message)
}

// TestDerivePodGroupRestoreNamesMembersByRank lists the members in spec order,
// as the status of a PodGroupSnapshot follows its spec, while the shared rules
// read them in the snapshot's rank order.
func TestDerivePodGroupRestoreNamesMembersByRank(t *testing.T) {
	spec := prSpec("c", "a", "b")
	observed := prPinning("c", "a", "b")
	observed.ranks = map[string]int{"a": 0, "b": 1, "c": 2}
	pinned := prDerive(spec, observed, snapshotv1alpha1.PodGroupRestoreStatus{})
	var ids []string
	for _, m := range pinned.Members {
		ids = append(ids, m.ID)
	}
	assert.Equal(t, []string{"c", "a", "b"}, ids, "the status follows the spec")

	for _, tc := range []struct {
		name     string
		observed podGroupRestoreObservation
		want     string
	}{
		{
			"a failure names the member's rank in the snapshot",
			prObserved(prPod("c", prStarted, prRestoreError), prPod("a", prStarted), prPod("b")),
			`member "c" (rank 2) failed with RestoreFailed: RestoreFailed message`,
		},
		{
			"the first failed member in rank order is named, not in spec order",
			prObserved(prPod("c", prStarted, prRestoreError), prPod("a", prStarted, prRestoreError), prPod("b")),
			`member "a" (rank 0) failed with RestoreFailed: RestoreFailed message`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.observed.ranks = observed.ranks
			failed := prCondition(prDerive(spec, tc.observed, pinned), snapshotv1alpha1.PodGroupConditionFailed)
			require.NotNil(t, failed)
			assert.Equal(t, tc.want, failed.Message)
		})
	}
}

func TestDerivePodGroupRestoreAdmitted(t *testing.T) {
	waiting := func(reason string) podGroupRestoreObservation {
		o := prObserved()
		o.admission = podGroupAdmission{podGroupAdmissionWaiting, reason, reason + " message"}
		return o
	}
	pinned := prStatus(prPinned("a", snapshotv1alpha1.RestorePhaseAdmitted), prPinned("b", snapshotv1alpha1.RestorePhaseAdmitted))

	for _, tc := range []struct {
		name       string
		observed   podGroupRestoreObservation
		previous   snapshotv1alpha1.PodGroupRestoreStatus
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{"waiting for a target Pod", waiting(snapshotv1alpha1.PodGroupReasonTargetPodPending), snapshotv1alpha1.PodGroupRestoreStatus{},
			metav1.ConditionFalse, snapshotv1alpha1.PodGroupReasonTargetPodPending},
		{"waiting for an agent", waiting(snapshotv1alpha1.PodGroupReasonAgentNotReady), snapshotv1alpha1.PodGroupRestoreStatus{},
			metav1.ConditionFalse, snapshotv1alpha1.PodGroupReasonAgentNotReady},
		{"admission passed pins the members", prPinning("a", "b"), snapshotv1alpha1.PodGroupRestoreStatus{},
			metav1.ConditionFalse, snapshotv1alpha1.PodGroupReasonAdmitting},
		{"pinned", prObserved(prPod("a"), prPod("b")), pinned, metav1.ConditionFalse, snapshotv1alpha1.PodGroupReasonAdmitting},
		{"the first startedAt commits", prObserved(prPod("a"), prPod("b", prStarted)), pinned,
			metav1.ConditionTrue, snapshotv1alpha1.PodGroupReasonCommitted},
		{
			"a refusal",
			func() podGroupRestoreObservation {
				o := prObserved()
				o.admission = podGroupAdmission{podGroupAdmissionRefused, snapshotv1alpha1.PodGroupReasonTargetInUse, "in use"}
				return o
			}(),
			snapshotv1alpha1.PodGroupRestoreStatus{},
			metav1.ConditionFalse, snapshotv1alpha1.PodGroupReasonTargetInUse,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := prDerive(prSpec("a", "b"), tc.observed, tc.previous)

			admitted := prCondition(got, snapshotv1alpha1.PodGroupConditionAdmitted)
			require.NotNil(t, admitted)
			assert.Equal(t, tc.wantStatus, admitted.Status)
			assert.Equal(t, tc.wantReason, admitted.Reason)
		})
	}
}

func TestDerivePodGroupRestoreReleasedAndReady(t *testing.T) {
	spec := prSpec("a", "b")
	restoring := prStatus(prPinned("a", snapshotv1alpha1.RestorePhaseRestoring), prPinned("b", snapshotv1alpha1.RestorePhaseRestoring))

	first := prDerive(spec, prObserved(prPod("a", prStarted), prPod("b", prStarted, prReleased)), restoring)
	released := prCondition(first, snapshotv1alpha1.PodGroupConditionReleased)
	require.NotNil(t, released, "Released turns True on the first Released member")
	assert.Equal(t, metav1.ConditionTrue, released.Status)
	assert.Equal(t, "Released", released.Reason)
	assert.Nil(t, prCondition(first, snapshotv1alpha1.PodGroupConditionReady))

	ready := prDerive(spec, prObserved(prPod("a", prStarted, prReleased), prPod("b", prStarted, prReleased)), first)
	cond := prCondition(ready, snapshotv1alpha1.PodGroupConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "Released", cond.Reason)
	assert.Equal(t, "all 2 members are released", cond.Message)
	assert.Equal(t, released.LastTransitionTime, prCondition(ready, snapshotv1alpha1.PodGroupConditionReleased).LastTransitionTime)
	assert.Nil(t, prCondition(ready, snapshotv1alpha1.PodGroupConditionFailed))

	t.Run("never withdrawn, and never failed by a later deadline or a lost Pod", func(t *testing.T) {
		observed := prObserved(podGroupRestoreMemberObservation{podDeleted: true}, prPod("b", prStarted, prRestoreError))
		observed.deadline = pgNow
		assert.Equal(t, ready, prDerive(spec, observed, ready))
	})

	t.Run("a member that fails after another's release fails the restore, which stays Released", func(t *testing.T) {
		got := prDerive(spec, prObserved(prPod("a", prStarted, prRestoreError), prPod("b", prStarted, prReleased)), first)
		failed := prCondition(got, snapshotv1alpha1.PodGroupConditionFailed)
		require.NotNil(t, failed)
		assert.Equal(t, snapshotv1alpha1.PodGroupReasonMemberFailed, failed.Reason)
		assert.True(t, meta.IsStatusConditionTrue(got.Conditions, snapshotv1alpha1.PodGroupConditionReleased))
		assert.Nil(t, prCondition(got, snapshotv1alpha1.PodGroupConditionReady))
	})
}

func TestDerivePodGroupRestoreFailureReasons(t *testing.T) {
	pinned := func(ids ...string) snapshotv1alpha1.PodGroupRestoreStatus {
		status := snapshotv1alpha1.PodGroupRestoreStatus{}
		for _, id := range ids {
			status.Members = append(status.Members, prPinned(id, snapshotv1alpha1.RestorePhaseAdmitted))
		}
		return status
	}
	// started observes a member that started, and failed with a group reason
	// when one is given; reported one that failed before it started.
	started := func(id string, groupReason ...string) podGroupRestoreMemberObservation {
		conditions := []corev1.PodCondition{prStarted}
		for _, reason := range groupReason {
			conditions = append(conditions, prRestoreError, prGroupFailed(reason))
		}
		return prPod(id, conditions...)
	}
	reported := func(id, groupReason string) podGroupRestoreMemberObservation {
		return prPod(id, prRestoreError, prGroupFailed(groupReason))
	}
	own := func(id string) podGroupRestoreMemberObservation { return prPod(id, prRestoreError) }
	deleted := podGroupRestoreMemberObservation{podDeleted: true}

	for _, tc := range []struct {
		name         string
		members      []podGroupRestoreMemberObservation
		previous     snapshotv1alpha1.PodGroupRestoreStatus
		admission    podGroupAdmission
		deleting     bool
		pastDeadline bool
		wantReason   string
		wantMessage  string
	}{
		{
			name:        "row 1: admission refused",
			admission:   podGroupAdmission{podGroupAdmissionRefused, snapshotv1alpha1.PodGroupReasonArtifactUnavailable, "not Ready"},
			wantReason:  snapshotv1alpha1.PodGroupReasonArtifactUnavailable,
			wantMessage: "not Ready",
		},
		{
			name:        "row 2: a started member failed on its own",
			members:     []podGroupRestoreMemberObservation{started("a"), own("b")},
			previous:    pinned("a", "b"),
			wantReason:  snapshotv1alpha1.PodGroupReasonMemberFailed,
			wantMessage: `member "b" (rank 1) failed with RestoreFailed: RestoreFailed message`,
		},
		{
			name:        "row 2: the session's CoordinatorLost",
			members:     []podGroupRestoreMemberObservation{started("a", snapshotv1alpha1.PodGroupReasonCoordinatorLost), started("b")},
			previous:    pinned("a", "b"),
			wantReason:  snapshotv1alpha1.PodGroupReasonCoordinatorLost,
			wantMessage: `member "a" (rank 0) failed with CoordinatorLost: CoordinatorLost message`,
		},
		{
			name:       "row 2: PodIPChanged after the commit",
			members:    []podGroupRestoreMemberObservation{started("a"), started("b", snapshotv1alpha1.PodGroupReasonPodIPChanged)},
			previous:   pinned("a", "b"),
			wantReason: snapshotv1alpha1.PodGroupReasonPodIPChanged,
		},
		{
			name:       "row 2: GateBudgetTooShort after the commit is MemberFailed",
			members:    []podGroupRestoreMemberObservation{started("a"), reported("b", snapshotv1alpha1.PodGroupReasonGateBudgetTooShort)},
			previous:   pinned("a", "b"),
			wantReason: snapshotv1alpha1.PodGroupReasonMemberFailed,
		},
		{
			name:     "row 2: a deleted target Pod in a committed restore, at once",
			members:  []podGroupRestoreMemberObservation{deleted, started("b")},
			previous: pinned("a", "b"),
			wantMessage: `member "a" (rank 0) failed with TargetPodDeleted: ` +
				`Pod "pod-a" was deleted or replaced before the member was released`,
			wantReason: snapshotv1alpha1.PodGroupReasonMemberFailed,
		},
		{
			name: "row 3: GateBudgetTooShort before the commit",
			members: []podGroupRestoreMemberObservation{
				reported("a", snapshotv1alpha1.PodGroupReasonGateBudgetTooShort), reported("b", snapshotv1alpha1.PodGroupReasonRefused),
			},
			previous:    pinned("a", "b"),
			wantReason:  snapshotv1alpha1.PodGroupReasonGateBudgetTooShort,
			wantMessage: `member "a" (rank 0) failed with GateBudgetTooShort: GateBudgetTooShort message`,
		},
		{
			name: "row 3: PodIPChanged before the commit",
			members: []podGroupRestoreMemberObservation{
				reported("a", snapshotv1alpha1.PodGroupReasonRefused), reported("b", snapshotv1alpha1.PodGroupReasonPodIPChanged),
			},
			previous:   pinned("a", "b"),
			wantReason: snapshotv1alpha1.PodGroupReasonPodIPChanged,
		},
		{
			name:        "row 3: any other reason before the commit is Refused",
			members:     []podGroupRestoreMemberObservation{reported("a", snapshotv1alpha1.PodGroupReasonRefused), own("b")},
			previous:    pinned("a", "b"),
			wantReason:  snapshotv1alpha1.PodGroupReasonRefused,
			wantMessage: `member "b" (rank 1) failed with RestoreFailed: RestoreFailed message`,
		},
		{
			name:     "row 6, not row 3: a deleted target Pod may hide its start",
			members:  []podGroupRestoreMemberObservation{deleted, reported("b", snapshotv1alpha1.PodGroupReasonRefused)},
			previous: pinned("a", "b"),
			wantMessage: `member "a" (rank 0) failed with TargetPodDeleted: ` +
				`Pod "pod-a" was deleted or replaced before the member was released`,
			wantReason: snapshotv1alpha1.PodGroupReasonMemberFailed,
		},
		{
			name:         "row 4: the deadline",
			members:      []podGroupRestoreMemberObservation{prPod("a", prStarted, prReleased), started("b")},
			previous:     pinned("a", "b"),
			pastDeadline: true,
			wantReason:   snapshotv1alpha1.PodGroupReasonDeadlineExceeded,
			wantMessage:  "the deadline of 600 seconds passed with 1 of 2 members not released",
		},
		{
			name:         "row 4: the deadline while admission waited",
			pastDeadline: true,
			previous: snapshotv1alpha1.PodGroupRestoreStatus{Conditions: []metav1.Condition{{
				Type: snapshotv1alpha1.PodGroupConditionAdmitted, Status: metav1.ConditionFalse,
				Reason: snapshotv1alpha1.PodGroupReasonTargetPodPending, Message: `target "a": Pod "pod-a" does not exist yet`,
			}}},
			wantReason:  snapshotv1alpha1.PodGroupReasonDeadlineExceeded,
			wantMessage: `the deadline of 600 seconds passed while admission was waiting: target "a": Pod "pod-a" does not exist yet`,
		},
		{
			name:        "row 5: deleted before any member started",
			members:     []podGroupRestoreMemberObservation{prPod("a"), prPod("b")},
			previous:    pinned("a", "b"),
			deleting:    true,
			wantReason:  snapshotv1alpha1.PodGroupReasonCancelled,
			wantMessage: podGroupRestoreCancelledMessage,
		},
		{
			name:         "the first cause is never rewritten",
			members:      []podGroupRestoreMemberObservation{started("a", snapshotv1alpha1.PodGroupReasonCoordinatorLost), started("b")},
			pastDeadline: true,
			previous: func() snapshotv1alpha1.PodGroupRestoreStatus {
				s := pinned("a", "b")
				s.Conditions = []metav1.Condition{{
					Type: snapshotv1alpha1.PodGroupConditionFailed, Status: metav1.ConditionTrue,
					Reason: snapshotv1alpha1.PodGroupReasonPodIPChanged, Message: "the first cause",
					LastTransitionTime: metav1.NewTime(pgNow.Add(-time.Hour)),
				}}
				return s
			}(),
			wantReason:  snapshotv1alpha1.PodGroupReasonPodIPChanged,
			wantMessage: "the first cause",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			members := tc.members
			if len(members) == 0 {
				members = make([]podGroupRestoreMemberObservation, 2)
			}
			observed := prObserved(members...)
			observed.admission, observed.deleting = tc.admission, tc.deleting
			if tc.pastDeadline {
				observed.deadline = pgNow
			}

			got := prDerive(prSpec("a", "b"), observed, tc.previous)

			failed := prCondition(got, snapshotv1alpha1.PodGroupConditionFailed)
			require.NotNil(t, failed)
			assert.Equal(t, metav1.ConditionTrue, failed.Status)
			assert.Equal(t, tc.wantReason, failed.Reason)
			if tc.wantMessage != "" {
				assert.Equal(t, tc.wantMessage, failed.Message)
			}
			if previous := meta.FindStatusCondition(tc.previous.Conditions, snapshotv1alpha1.PodGroupConditionFailed); previous != nil {
				assert.Equal(t, previous.LastTransitionTime, failed.LastTransitionTime)
			}
		})
	}
}

func TestDerivePodGroupRestoreNoFailure(t *testing.T) {
	pinned := prStatus(prPinned("a", snapshotv1alpha1.RestorePhaseAdmitted), prPinned("b", snapshotv1alpha1.RestorePhaseAdmitted))
	for _, tc := range []struct {
		name     string
		members  []podGroupRestoreMemberObservation
		deleting bool
	}{
		{name: "members still restoring", members: []podGroupRestoreMemberObservation{prPod("a", prStarted), prPod("b")}},
		{name: "deleted after a member started", members: []podGroupRestoreMemberObservation{prPod("a", prStarted), prPod("b")}, deleting: true},
		{
			name:    "a member failed on its own before any member started, while another may still start",
			members: []podGroupRestoreMemberObservation{prPod("a", prRestoreError), prPod("b")},
		},
		{
			name:    "a deleted target Pod before the commit, while another may still start",
			members: []podGroupRestoreMemberObservation{{podDeleted: true}, prPod("b")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observed := prObserved(tc.members...)
			observed.deleting = tc.deleting
			got := prDerive(prSpec("a", "b"), observed, pinned)
			assert.Nil(t, prCondition(got, snapshotv1alpha1.PodGroupConditionFailed))
			assert.Nil(t, prCondition(got, snapshotv1alpha1.PodGroupConditionReady))
		})
	}
}

func TestDerivePodGroupRestoreDeadline(t *testing.T) {
	spec := prSpec("a", "b", "c")
	previous := prStatus(prPinned("a", snapshotv1alpha1.RestorePhaseRestoring), prPinned("b", snapshotv1alpha1.RestorePhaseRestoring),
		prPinned("c", snapshotv1alpha1.RestorePhaseAdmitted))
	observed := prObserved(prPod("a", prStarted, prReleased), prPod("b", prStarted), prPod("c"))
	observed.deadline = pgNow

	got := prDerive(spec, observed, previous)

	assert.Equal(t, snapshotv1alpha1.RestorePhaseReleased, got.Members[0].Phase)
	for _, m := range got.Members[1:] {
		assert.Equal(t, snapshotv1alpha1.RestorePhaseFailed, m.Phase, m.ID)
		assert.Equal(t, snapshotv1alpha1.PodGroupReasonDeadlineExceeded, m.Reason, m.ID)
		assert.Equal(t, podGroupRestoreDeadlineMemberMessage, m.Message, m.ID)
	}
	assert.NotNil(t, got.Members[1].StartedAt, "startedAt survives the deadline")
	failed := prCondition(got, snapshotv1alpha1.PodGroupConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, snapshotv1alpha1.PodGroupReasonDeadlineExceeded, failed.Reason)
	assert.True(t, meta.IsStatusConditionTrue(got.Conditions, snapshotv1alpha1.PodGroupConditionReleased))
	assert.True(t, podGroupRestoreFinished(&got))

	// A later member failure changes neither the members nor the restore.
	observed.members[1] = prPod("b", prStarted, prRestoreError)
	assert.Equal(t, got, prDerive(spec, observed, got))
}

func TestDerivePodGroupRestoreIsStable(t *testing.T) {
	spec := prSpec("a", "b")
	observed := prPinning("a", "b")
	pinned := prDerive(spec, observed, snapshotv1alpha1.PodGroupRestoreStatus{})
	running := prObserved(prPod("a", prStarted, prReleased), prPod("b", prStarted))
	first := prDerive(spec, running, pinned)
	second := derivePodGroupRestoreStatus(spec, running, first, pgNow.Add(time.Minute))
	assert.Equal(t, first, second, "deriving again from the same objects changes nothing, timestamps included")
}

func TestDerivePodGroupRestoreRefusalGivesEveryMemberTheFixedMessage(t *testing.T) {
	long := "x" + strings.Repeat("é", maxConditionMessageLength)
	observed := prObserved()
	observed.admission = podGroupRefusal(long)

	got := prDerive(prSpec("a", "b"), observed, snapshotv1alpha1.PodGroupRestoreStatus{})

	for _, condType := range []string{snapshotv1alpha1.PodGroupConditionFailed, snapshotv1alpha1.PodGroupConditionAdmitted} {
		cond := prCondition(got, condType)
		require.NotNil(t, cond, condType)
		assert.Equal(t, long[:maxConditionMessageLength-1], cond.Message, condType)
	}
	for _, m := range got.Members {
		assert.Equal(t, snapshotv1alpha1.PodGroupReasonRefused, m.Reason, m.ID)
		assert.Equal(t, podGroupRestoreRefusedMemberMessage, m.Message, m.ID)
		assert.Nil(t, m.Pod, m.ID)
	}
}

func TestPodGroupRestoreFinished(t *testing.T) {
	failed := metav1.Condition{Type: snapshotv1alpha1.PodGroupConditionFailed, Status: metav1.ConditionTrue, Reason: "MemberFailed"}
	ready := metav1.Condition{Type: snapshotv1alpha1.PodGroupConditionReady, Status: metav1.ConditionTrue, Reason: "Released"}
	released := metav1.Condition{Type: snapshotv1alpha1.PodGroupConditionReleased, Status: metav1.ConditionTrue, Reason: "Released"}
	for _, tc := range []struct {
		name       string
		conditions []metav1.Condition
		phases     []snapshotv1alpha1.RestorePhase
		want       bool
	}{
		{"no status yet", nil, nil, false},
		{"waiting", nil, []snapshotv1alpha1.RestorePhase{"Pending", "Pending"}, false},
		{"running", nil, []snapshotv1alpha1.RestorePhase{"Restoring", "Admitted"}, false},
		{"released, not ready", []metav1.Condition{released}, []snapshotv1alpha1.RestorePhase{"Released", "Restoring"}, false},
		{"ready", []metav1.Condition{released, ready}, []snapshotv1alpha1.RestorePhase{"Released", "Released"}, true},
		{"failed while a member restores", []metav1.Condition{failed}, []snapshotv1alpha1.RestorePhase{"Failed", "Restoring"}, false},
		{"failed with a member never started", []metav1.Condition{failed}, []snapshotv1alpha1.RestorePhase{"Failed", "Admitted"}, true},
		{"cancelled", []metav1.Condition{failed}, []snapshotv1alpha1.RestorePhase{"Failed", "Failed"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := &snapshotv1alpha1.PodGroupRestoreStatus{Conditions: tc.conditions}
			for _, phase := range tc.phases {
				status.Members = append(status.Members, snapshotv1alpha1.PodGroupRestoreMemberStatus{Phase: phase})
			}
			assert.Equal(t, tc.want, podGroupRestoreFinished(status))
		})
	}
}

func TestPodGroupRestoreReportOf(t *testing.T) {
	conditions := []corev1.PodCondition{prStarted, prRestoreError, prGroupFailed(snapshotv1alpha1.PodGroupReasonRefused)}
	pod := prReportedPod("a", conditions...)
	assert.Equal(t, podGroupRestoreReport{started: &pod.Status.Conditions[1], restored: &pod.Status.Conditions[2],
		groupFailed: &pod.Status.Conditions[3]}, podGroupRestoreReportOf(pod))
	assert.Equal(t, podGroupRestoreReport{}, podGroupRestoreReportOf(prReportedPod("a")), "a Pod with no report")
}

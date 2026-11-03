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

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

var (
	pgNow       = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	pgStartedAt = metav1.NewTime(pgNow.Add(-time.Minute))
)

func pgSpec(ids ...string) *snapshotv1alpha1.PodGroupSnapshotSpec {
	spec := &snapshotv1alpha1.PodGroupSnapshotSpec{DeadlineSeconds: 600}
	for _, id := range ids {
		spec.Members = append(spec.Members, snapshotv1alpha1.PodGroupSnapshotMember{
			ID: id,
			Source: snapshotv1alpha1.PodSnapshotSource{PodRef: snapshotv1alpha1.PodReference{
				Name: "pod-" + id, UID: types.UID("pod-uid-" + id), Containers: []string{"main"},
			}},
		})
	}
	return spec
}

func pgCondition(condType, reason string) metav1.Condition {
	return metav1.Condition{Type: condType, Status: metav1.ConditionTrue, Reason: reason, Message: reason + " message"}
}

var (
	pgReady   = pgCondition(snapshotv1alpha1.PodSnapshotConditionReady, "Ready")
	pgStarted = metav1.Condition{Type: snapshotv1alpha1.PodSnapshotConditionStarted, Status: metav1.ConditionTrue,
		Reason: "Started", LastTransitionTime: pgStartedAt}
)

func pgFailed(reason string) metav1.Condition {
	return pgCondition(snapshotv1alpha1.PodSnapshotConditionFailed, reason)
}

// pgSnap is the PodSnapshot of member id, bound to content-<id>.
func pgSnap(id string, conditions ...metav1.Condition) *snapshotv1alpha1.PodSnapshot {
	return &snapshotv1alpha1.PodSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "group-" + id, Namespace: "ns", UID: types.UID("snap-uid-" + id)},
		Status: snapshotv1alpha1.PodSnapshotStatus{
			BoundPodSnapshotContentName: ptr.To("content-" + id),
			Conditions:                  conditions,
		},
	}
}

func pgContent(id string, conditions ...metav1.Condition) *snapshotv1alpha1.PodSnapshotContent {
	return &snapshotv1alpha1.PodSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "content-" + id, UID: types.UID("content-uid-" + id)},
		Status:     snapshotv1alpha1.PodSnapshotContentStatus{Conditions: conditions},
	}
}

// pgCheckpointed observes a member whose snapshot is complete and restorable.
func pgCheckpointed(id string) podGroupMemberObservation {
	return podGroupMemberObservation{
		podSnapshot: pgSnap(id, pgReady),
		content:     pgContent(id, pgStarted, pgReady),
		nodeName:    "node-" + id,
	}
}

// pgObserved builds an observation of the given members with a deadline an hour away.
func pgObserved(members ...podGroupMemberObservation) podGroupSnapshotObservation {
	return podGroupSnapshotObservation{members: members, deadline: pgNow.Add(time.Hour)}
}

func pgDerive(spec *snapshotv1alpha1.PodGroupSnapshotSpec, observed podGroupSnapshotObservation,
	previous snapshotv1alpha1.PodGroupSnapshotStatus,
) snapshotv1alpha1.PodGroupSnapshotStatus {
	return derivePodGroupSnapshotStatus(spec, observed, previous, pgNow)
}

// pgPinned is a member status entry as an earlier reconcile wrote it.
func pgPinned(id string, rank int32, phase snapshotv1alpha1.CheckpointPhase) snapshotv1alpha1.PodGroupSnapshotMemberStatus {
	m := snapshotv1alpha1.PodGroupSnapshotMemberStatus{ID: id, Rank: rank, Phase: phase, NodeName: "node-" + id}
	if phase != snapshotv1alpha1.CheckpointPhasePending {
		m.PodSnapshot = &snapshotv1alpha1.ObjectIdentity{Name: "group-" + id, UID: types.UID("snap-uid-" + id)}
	}
	if phase == snapshotv1alpha1.CheckpointPhaseCheckpointing || phase == snapshotv1alpha1.CheckpointPhaseCheckpointed {
		m.StartedAt = pgStartedAt.DeepCopy()
	}
	if phase == snapshotv1alpha1.CheckpointPhaseCheckpointed {
		m.Content = &snapshotv1alpha1.ObjectIdentity{Name: "content-" + id, UID: types.UID("content-uid-" + id)}
	}
	return m
}

// pgDeleted is m failed because the API server confirmed its PodSnapshot is gone.
func pgDeleted(m snapshotv1alpha1.PodGroupSnapshotMemberStatus) snapshotv1alpha1.PodGroupSnapshotMemberStatus {
	m.Phase = snapshotv1alpha1.CheckpointPhaseFailed
	m.Reason = snapshotv1alpha1.ReasonPodSnapshotDeleted
	m.Message = `PodSnapshot "group-` + m.ID + `" was deleted before the member was checkpointed`
	return m
}

func TestDerivePodGroupSnapshotMemberTable(t *testing.T) {
	refused := podGroupAdmission{podGroupAdmissionRefused, snapshotv1alpha1.PodGroupReasonRefused, "source Pod is gone"}
	otherStart := metav1.NewTime(pgNow.Add(-time.Second))

	for _, tc := range []struct {
		name         string
		previous     *snapshotv1alpha1.PodGroupSnapshotMemberStatus
		member       podGroupMemberObservation
		admission    podGroupAdmission
		deleting     bool
		pastDeadline bool
		want         snapshotv1alpha1.PodGroupSnapshotMemberStatus
	}{
		{
			name: "nothing observed is Pending",
			want: snapshotv1alpha1.PodGroupSnapshotMemberStatus{Phase: snapshotv1alpha1.CheckpointPhasePending},
		},
		{
			name:   "a PodSnapshot is Admitted and pins it",
			member: podGroupMemberObservation{podSnapshot: pgSnap("a"), nodeName: "node-a"},
			want:   pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseAdmitted),
		},
		{
			name:   "Started on the content is Checkpointing and copies startedAt",
			member: podGroupMemberObservation{podSnapshot: pgSnap("a"), content: pgContent("a", pgStarted), nodeName: "node-a"},
			want:   pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseCheckpointing),
		},
		{
			name:   "a Ready PodSnapshot with its content is Checkpointed and pins the content",
			member: pgCheckpointed("a"),
			want:   pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseCheckpointed),
		},
		{
			name: "a Ready PodSnapshot whose content is not seen yet stays where it was",
			member: podGroupMemberObservation{
				podSnapshot: pgSnap("a", pgReady), nodeName: "node-a",
			},
			want: pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseAdmitted),
		},
		{
			name:   "a Failed PodSnapshot fails the member with its reason and message",
			member: podGroupMemberObservation{podSnapshot: pgSnap("a", pgFailed("SourcePodGone")), nodeName: "node-a"},
			want: func() snapshotv1alpha1.PodGroupSnapshotMemberStatus {
				m := pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseFailed)
				m.Reason, m.Message = "SourcePodGone", "SourcePodGone message"
				return m
			}(),
		},
		{
			name: "a member that fails straight from Admitted keeps its startedAt",
			member: podGroupMemberObservation{
				podSnapshot: pgSnap("a", pgFailed("CudaCheckpointFailed")),
				content:     pgContent("a", pgStarted, pgFailed("CudaCheckpointFailed")),
				nodeName:    "node-a",
			},
			previous: ptr.To(pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseAdmitted)),
			want: func() snapshotv1alpha1.PodGroupSnapshotMemberStatus {
				m := pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseFailed)
				m.StartedAt = pgStartedAt.DeepCopy()
				m.Reason, m.Message = "CudaCheckpointFailed", "CudaCheckpointFailed message"
				return m
			}(),
		},
		{
			name:         "the deadline fails an unfinished member",
			member:       podGroupMemberObservation{podSnapshot: pgSnap("a"), nodeName: "node-a"},
			pastDeadline: true,
			want: func() snapshotv1alpha1.PodGroupSnapshotMemberStatus {
				m := pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseFailed)
				m.Reason = snapshotv1alpha1.PodGroupReasonDeadlineExceeded
				m.Message = "the deadline of 600 seconds passed before the member was checkpointed"
				return m
			}(),
		},
		{
			name:         "the deadline leaves a member whose PodSnapshot is Ready Checkpointed",
			member:       pgCheckpointed("a"),
			pastDeadline: true,
			want:         pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseCheckpointed),
		},
		{
			name:         "a Failed PodSnapshot outranks the deadline",
			member:       podGroupMemberObservation{podSnapshot: pgSnap("a", pgFailed("CoordinatorLost")), nodeName: "node-a"},
			pastDeadline: true,
			want: func() snapshotv1alpha1.PodGroupSnapshotMemberStatus {
				m := pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseFailed)
				m.Reason, m.Message = "CoordinatorLost", "CoordinatorLost message"
				return m
			}(),
		},
		{
			name:     "deletion before any member started cancels the member",
			member:   podGroupMemberObservation{podSnapshot: pgSnap("a"), nodeName: "node-a"},
			deleting: true,
			want: func() snapshotv1alpha1.PodGroupSnapshotMemberStatus {
				m := pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseFailed)
				m.Reason, m.Message = snapshotv1alpha1.PodGroupReasonCancelled, podGroupCancelledMessage
				return m
			}(),
		},
		{
			name:     "deletion after the member started leaves it Checkpointing",
			member:   podGroupMemberObservation{podSnapshot: pgSnap("a"), content: pgContent("a", pgStarted), nodeName: "node-a"},
			deleting: true,
			want:     pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseCheckpointing),
		},
		{
			name:      "a refused group fails the member with the refusal",
			member:    podGroupMemberObservation{nodeName: "node-a"},
			admission: refused,
			want: snapshotv1alpha1.PodGroupSnapshotMemberStatus{
				NodeName: "node-a", Phase: snapshotv1alpha1.CheckpointPhaseFailed,
				Reason: snapshotv1alpha1.PodGroupReasonRefused, Message: "source Pod is gone",
			},
		},
		{
			name:     "Failed is final",
			previous: ptr.To(pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseFailed)),
			member:   pgCheckpointed("a"),
			want: func() snapshotv1alpha1.PodGroupSnapshotMemberStatus {
				m := pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseFailed)
				m.StartedAt = pgStartedAt.DeepCopy() // write-once fields still fill in
				return m
			}(),
		},
		{
			name:     "Checkpointed is final",
			previous: ptr.To(pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseCheckpointed)),
			member: podGroupMemberObservation{
				podSnapshot: pgSnap("a", pgFailed("Late")), content: pgContent("a", pgFailed("Late")),
			},
			pastDeadline: true,
			want:         pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseCheckpointed),
		},
		{
			name:     "a PodSnapshot that replaced the pinned one is ignored",
			previous: ptr.To(pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseAdmitted)),
			member: func() podGroupMemberObservation {
				snap := pgSnap("a", pgFailed("Replaced"))
				snap.UID = "another-uid"
				return podGroupMemberObservation{podSnapshot: snap, content: pgContent("a", pgStarted)}
			}(),
			want: pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseAdmitted),
		},
		{
			name:     "startedAt and nodeName are written once",
			previous: ptr.To(pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseCheckpointing)),
			member: podGroupMemberObservation{
				podSnapshot: pgSnap("a"),
				content: pgContent("a", metav1.Condition{Type: snapshotv1alpha1.PodSnapshotConditionStarted,
					Status: metav1.ConditionTrue, Reason: "Started", LastTransitionTime: otherStart}),
				nodeName: "node-elsewhere",
			},
			want: pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseCheckpointing),
		},
		{
			name:     "a phase never moves back when its PodSnapshot and content are gone",
			previous: ptr.To(pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseCheckpointing)),
			want:     pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseCheckpointing),
		},
		{
			name:     "a pinned PodSnapshot confirmed gone fails the member with PodSnapshotDeleted",
			previous: ptr.To(pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseCheckpointing)),
			member:   podGroupMemberObservation{podSnapshotDeleted: true},
			want:     pgDeleted(pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseCheckpointing)),
		},
		{
			name:         "PodSnapshotDeleted outranks the deadline",
			previous:     ptr.To(pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseAdmitted)),
			member:       podGroupMemberObservation{podSnapshotDeleted: true},
			pastDeadline: true,
			want:         pgDeleted(pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseAdmitted)),
		},
		{
			name:     "a Checkpointed member whose PodSnapshot is deleted stays Checkpointed",
			previous: ptr.To(pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseCheckpointed)),
			member:   podGroupMemberObservation{podSnapshotDeleted: true},
			want:     pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseCheckpointed),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observed := pgObserved(tc.member)
			observed.admission, observed.deleting = tc.admission, tc.deleting
			if tc.pastDeadline {
				observed.deadline = pgNow
			}
			var previous snapshotv1alpha1.PodGroupSnapshotStatus
			if tc.previous != nil {
				previous.Members = []snapshotv1alpha1.PodGroupSnapshotMemberStatus{*tc.previous}
			}
			want := tc.want
			want.ID = "a"

			got := pgDerive(pgSpec("a"), observed, previous)

			require.Len(t, got.Members, 1)
			assert.Equal(t, want, got.Members[0])
		})
	}
}

func TestDerivePodGroupSnapshotRankAndOrder(t *testing.T) {
	previous := snapshotv1alpha1.PodGroupSnapshotStatus{Members: []snapshotv1alpha1.PodGroupSnapshotMemberStatus{
		pgPinned("b", 1, snapshotv1alpha1.CheckpointPhaseAdmitted),
	}}
	got := pgDerive(pgSpec("a", "b", "c"), pgObserved(), previous)

	require.Len(t, got.Members, 3)
	for rank, id := range []string{"a", "b", "c"} {
		assert.Equal(t, id, got.Members[rank].ID)
		assert.Equal(t, int32(rank), got.Members[rank].Rank)
	}
	assert.Equal(t, snapshotv1alpha1.CheckpointPhaseAdmitted, got.Members[1].Phase, "entries are kept by ID")
}

func TestDerivePodGroupSnapshotAdmitted(t *testing.T) {
	waiting := podGroupAdmission{podGroupAdmissionWaiting, snapshotv1alpha1.PodGroupReasonAgentNotReady,
		`node "node-a": no node agent Pod runs on it`}
	for _, tc := range []struct {
		name       string
		observed   podGroupSnapshotObservation
		wantStatus metav1.ConditionStatus
		wantReason string
		wantMsg    string
	}{
		{
			name:       "waiting on an agent",
			observed:   func() podGroupSnapshotObservation { o := pgObserved(); o.admission = waiting; return o }(),
			wantStatus: metav1.ConditionFalse,
			wantReason: snapshotv1alpha1.PodGroupReasonAgentNotReady,
			wantMsg:    waiting.message,
		},
		{
			name: "admission passed, before the PodSnapshots exist",
			observed: func() podGroupSnapshotObservation {
				o := pgObserved()
				o.admission.state = podGroupAdmissionPassed
				return o
			}(),
			wantStatus: metav1.ConditionFalse,
			wantReason: snapshotv1alpha1.PodGroupReasonAdmitting,
		},
		{
			name:       "member work exists",
			observed:   pgObserved(podGroupMemberObservation{podSnapshot: pgSnap("a")}),
			wantStatus: metav1.ConditionFalse,
			wantReason: snapshotv1alpha1.PodGroupReasonAdmitting,
		},
		{
			name: "the first startedAt commits",
			observed: pgObserved(
				podGroupMemberObservation{podSnapshot: pgSnap("a")},
				podGroupMemberObservation{podSnapshot: pgSnap("b"), content: pgContent("b", pgStarted)},
			),
			wantStatus: metav1.ConditionTrue,
			wantReason: snapshotv1alpha1.PodGroupReasonCommitted,
		},
		{
			name: "a refusal",
			observed: func() podGroupSnapshotObservation {
				o := pgObserved()
				o.admission = podGroupAdmission{podGroupAdmissionRefused, snapshotv1alpha1.PodGroupReasonRefused, "duplicate source"}
				return o
			}(),
			wantStatus: metav1.ConditionFalse,
			wantReason: snapshotv1alpha1.PodGroupReasonRefused,
			wantMsg:    "duplicate source",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := pgDerive(pgSpec("a", "b"), tc.observed, snapshotv1alpha1.PodGroupSnapshotStatus{})

			admitted := meta.FindStatusCondition(got.Conditions, snapshotv1alpha1.PodGroupConditionAdmitted)
			require.NotNil(t, admitted)
			assert.Equal(t, tc.wantStatus, admitted.Status)
			assert.Equal(t, tc.wantReason, admitted.Reason)
			if tc.wantMsg != "" {
				assert.Equal(t, tc.wantMsg, admitted.Message)
			}
		})
	}
}

func TestDerivePodGroupSnapshotCommittedIsPermanent(t *testing.T) {
	spec := pgSpec("a", "b")
	first := pgDerive(spec, pgObserved(
		podGroupMemberObservation{podSnapshot: pgSnap("a"), content: pgContent("a", pgStarted)},
		podGroupMemberObservation{podSnapshot: pgSnap("b")},
	), snapshotv1alpha1.PodGroupSnapshotStatus{})
	require.True(t, meta.IsStatusConditionTrue(first.Conditions, snapshotv1alpha1.PodGroupConditionAdmitted))

	// The member's content is gone from the cache, and member b fails: still committed.
	second := pgDerive(spec, pgObserved(
		podGroupMemberObservation{},
		podGroupMemberObservation{podSnapshot: pgSnap("b", pgFailed("CoordinatorLost"))},
	), first)
	admitted := meta.FindStatusCondition(second.Conditions, snapshotv1alpha1.PodGroupConditionAdmitted)
	require.NotNil(t, admitted)
	assert.Equal(t, metav1.ConditionTrue, admitted.Status)
	assert.Equal(t, snapshotv1alpha1.PodGroupReasonCommitted, admitted.Reason)
}

// pgStartedMember observes a member whose agent recorded Started, and that
// failed with reason when one is given.
func pgStartedMember(id string, failed ...string) podGroupMemberObservation {
	conditions := []metav1.Condition{pgStarted}
	snapConditions := []metav1.Condition{}
	for _, reason := range failed {
		conditions = append(conditions, pgFailed(reason))
		snapConditions = append(snapConditions, pgFailed(reason))
	}
	return podGroupMemberObservation{podSnapshot: pgSnap(id, snapConditions...), content: pgContent(id, conditions...)}
}

// pgFailedMember observes a member whose PodSnapshot failed before it started.
func pgFailedMember(id, reason string) podGroupMemberObservation {
	return podGroupMemberObservation{podSnapshot: pgSnap(id, pgFailed(reason))}
}

// pgAdmittedMember observes a member whose PodSnapshot exists, with no result.
func pgAdmittedMember(id string) podGroupMemberObservation {
	return podGroupMemberObservation{podSnapshot: pgSnap(id)}
}

func TestDerivePodGroupSnapshotFailureReasons(t *testing.T) {
	started, failedOnly, admitted := pgStartedMember, pgFailedMember, pgAdmittedMember

	for _, tc := range []struct {
		name         string
		members      []podGroupMemberObservation
		admission    podGroupAdmission
		deleting     bool
		pastDeadline bool
		previous     snapshotv1alpha1.PodGroupSnapshotStatus
		wantReason   string
		wantMessage  string
	}{
		{
			name:        "admission refused",
			admission:   podGroupAdmission{podGroupAdmissionRefused, snapshotv1alpha1.PodGroupReasonRefused, "duplicate source"},
			wantReason:  snapshotv1alpha1.PodGroupReasonRefused,
			wantMessage: "duplicate source",
		},
		{
			name:        "deleted while no member has startedAt",
			members:     []podGroupMemberObservation{admitted("a"), admitted("b"), {}},
			deleting:    true,
			wantReason:  snapshotv1alpha1.PodGroupReasonCancelled,
			wantMessage: podGroupCancelledMessage,
		},
		{
			name:         "deadline passed before Ready",
			members:      []podGroupMemberObservation{pgCheckpointed("a"), started("b"), {}},
			pastDeadline: true,
			wantReason:   snapshotv1alpha1.PodGroupReasonDeadlineExceeded,
			wantMessage:  "the deadline of 600 seconds passed with 2 of 3 members not checkpointed",
		},
		{
			name:         "deadline passed while admission waited",
			pastDeadline: true,
			previous: snapshotv1alpha1.PodGroupSnapshotStatus{Conditions: []metav1.Condition{{
				Type: snapshotv1alpha1.PodGroupConditionAdmitted, Status: metav1.ConditionFalse,
				Reason: snapshotv1alpha1.PodGroupReasonAgentNotReady, Message: `node "node-a": agent Pod "agent-1" is not Ready`,
			}}},
			wantReason: snapshotv1alpha1.PodGroupReasonDeadlineExceeded,
			wantMessage: `the deadline of 600 seconds passed while admission was waiting: ` +
				`node "node-a": agent Pod "agent-1" is not Ready`,
		},
		{
			name:        "not committed, and every member reported",
			members:     []podGroupMemberObservation{failedOnly("a", "Refused"), failedOnly("b", "SourcePodNotFound"), failedOnly("c", "Refused")},
			wantReason:  snapshotv1alpha1.PodGroupReasonRefused,
			wantMessage: `member "b" (rank 1) failed with SourcePodNotFound: SourcePodNotFound message`,
		},
		{
			name:        "b's own cause is named over a's group reason, before the commit",
			members:     []podGroupMemberObservation{failedOnly("a", "Refused"), failedOnly("b", "CriuDumpFailed")},
			wantReason:  snapshotv1alpha1.PodGroupReasonRefused,
			wantMessage: `member "b" (rank 1) failed with CriuDumpFailed: CriuDumpFailed message`,
		},
		{
			name:        "b's own cause is named over a's group reason, after the commit",
			members:     []podGroupMemberObservation{started("a", "CoordinatorLost"), started("b", "CriuDumpFailed")},
			wantReason:  snapshotv1alpha1.PodGroupReasonMemberFailed,
			wantMessage: `member "b" (rank 1) failed with CriuDumpFailed: CriuDumpFailed message`,
		},
		{
			name:        "a started member failed",
			members:     []podGroupMemberObservation{started("a"), started("b", "CriuDumpFailed"), admitted("c")},
			wantReason:  snapshotv1alpha1.PodGroupReasonMemberFailed,
			wantMessage: `member "b" (rank 1) failed with CriuDumpFailed: CriuDumpFailed message`,
		},
		{
			name:       "a member that never started failed after another started",
			members:    []podGroupMemberObservation{started("a"), failedOnly("b", "AgentRestarted")},
			wantReason: snapshotv1alpha1.PodGroupReasonMemberFailed,
		},
		{
			name:       "a member lost the coordinator after the commit",
			members:    []podGroupMemberObservation{started("a", "CoordinatorLost"), started("b")},
			wantReason: snapshotv1alpha1.PodGroupReasonCoordinatorLost,
		},
		{
			name:       "a member reported the deadline after the commit",
			members:    []podGroupMemberObservation{started("a", "DeadlineExceeded"), started("b")},
			wantReason: snapshotv1alpha1.PodGroupReasonDeadlineExceeded,
		},
		{
			name:       "a member reported a cancellation after the commit",
			members:    []podGroupMemberObservation{started("a"), started("b", "Cancelled")},
			wantReason: snapshotv1alpha1.PodGroupReasonCancelled,
		},
		{
			name:        "the first failed member in rank order is named",
			members:     []podGroupMemberObservation{started("a"), started("b", "First"), started("c", "Second")},
			wantReason:  snapshotv1alpha1.PodGroupReasonMemberFailed,
			wantMessage: `member "b" (rank 1) failed with First: First message`,
		},
		{
			name:         "a member failure seen with the deadline wins, since it came first",
			members:      []podGroupMemberObservation{started("a", "CriuDumpFailed"), started("b")},
			pastDeadline: true,
			wantReason:   snapshotv1alpha1.PodGroupReasonMemberFailed,
			wantMessage:  `member "a" (rank 0) failed with CriuDumpFailed: CriuDumpFailed message`,
		},
		{
			name:         "an own failure seen with the deadline, while another member never reported",
			members:      []podGroupMemberObservation{admitted("a"), failedOnly("b", "SourcePodGone")},
			pastDeadline: true,
			wantReason:   snapshotv1alpha1.PodGroupReasonDeadlineExceeded,
			wantMessage: `the deadline of 600 seconds passed with 2 of 2 members not checkpointed; ` +
				`member "b" (rank 1) failed with SourcePodGone: SourcePodGone message`,
		},
		{
			name:         "an own failure seen with the deadline, when every member had reported",
			members:      []podGroupMemberObservation{failedOnly("a", "Refused"), failedOnly("b", "SourcePodGone")},
			pastDeadline: true,
			wantReason:   snapshotv1alpha1.PodGroupReasonRefused,
			wantMessage:  `member "b" (rank 1) failed with SourcePodGone: SourcePodGone message`,
		},
		{
			name:        "deleted after a member failed, while another never reported",
			members:     []podGroupMemberObservation{failedOnly("a", "SourcePodGone"), admitted("b")},
			deleting:    true,
			wantReason:  snapshotv1alpha1.PodGroupReasonCancelled,
			wantMessage: podGroupCancelledMessage,
		},
		{
			name:    "a deleted PodSnapshot in a committed group, at once",
			members: []podGroupMemberObservation{{podSnapshotDeleted: true}, started("b")},
			previous: snapshotv1alpha1.PodGroupSnapshotStatus{Members: []snapshotv1alpha1.PodGroupSnapshotMemberStatus{
				pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseCheckpointing),
			}},
			wantReason: snapshotv1alpha1.PodGroupReasonMemberFailed,
			wantMessage: `member "a" (rank 0) failed with PodSnapshotDeleted: ` +
				`PodSnapshot "group-a" was deleted before the member was checkpointed`,
		},
		{
			name:    "a deleted PodSnapshot and own failures, none started, at once",
			members: []podGroupMemberObservation{{podSnapshotDeleted: true}, failedOnly("b", "Refused")},
			previous: snapshotv1alpha1.PodGroupSnapshotStatus{Members: []snapshotv1alpha1.PodGroupSnapshotMemberStatus{
				pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseAdmitted),
			}},
			wantReason: snapshotv1alpha1.PodGroupReasonMemberFailed,
			wantMessage: `member "a" (rank 0) failed with PodSnapshotDeleted: ` +
				`PodSnapshot "group-a" was deleted before the member was checkpointed`,
		},
		{
			name:         "the first cause is never rewritten",
			members:      []podGroupMemberObservation{started("a", "CoordinatorLost"), started("b")},
			pastDeadline: true,
			deleting:     true,
			previous: snapshotv1alpha1.PodGroupSnapshotStatus{Conditions: []metav1.Condition{{
				Type: snapshotv1alpha1.PodGroupConditionFailed, Status: metav1.ConditionTrue,
				Reason: snapshotv1alpha1.PodGroupReasonMemberFailed, Message: "the first cause",
				LastTransitionTime: metav1.NewTime(pgNow.Add(-time.Hour)),
			}}},
			wantReason:  snapshotv1alpha1.PodGroupReasonMemberFailed,
			wantMessage: "the first cause",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			members := tc.members
			if len(members) == 0 {
				members = make([]podGroupMemberObservation, 3)
			}
			observed := pgObserved(members...)
			observed.admission, observed.deleting = tc.admission, tc.deleting
			if tc.pastDeadline {
				observed.deadline = pgNow
			}
			ids := []string{"a", "b", "c"}[:len(members)]

			got := pgDerive(pgSpec(ids...), observed, tc.previous)

			failed := meta.FindStatusCondition(got.Conditions, snapshotv1alpha1.PodGroupConditionFailed)
			require.NotNil(t, failed)
			assert.Equal(t, metav1.ConditionTrue, failed.Status)
			assert.Equal(t, tc.wantReason, failed.Reason)
			if tc.wantMessage != "" {
				assert.Equal(t, tc.wantMessage, failed.Message)
			}
			if previous := meta.FindStatusCondition(tc.previous.Conditions, snapshotv1alpha1.PodGroupConditionFailed); previous != nil {
				assert.Equal(t, previous.LastTransitionTime, failed.LastTransitionTime)
			}
			assert.Nil(t, meta.FindStatusCondition(got.Conditions, snapshotv1alpha1.PodGroupConditionReady))
		})
	}
}

func TestDerivePodGroupSnapshotNoFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		members  []podGroupMemberObservation
		deleting bool
	}{
		{name: "members still running", members: []podGroupMemberObservation{{podSnapshot: pgSnap("a")}, {}}},
		{
			name: "deleted after a member started",
			members: []podGroupMemberObservation{
				{podSnapshot: pgSnap("a"), content: pgContent("a", pgStarted)}, {podSnapshot: pgSnap("b")},
			},
			deleting: true,
		},
		{
			name:    "a member failed on its own before any member started, while another runs",
			members: []podGroupMemberObservation{pgFailedMember("a", "SourcePodGone"), pgAdmittedMember("b")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observed := pgObserved(tc.members...)
			observed.deleting = tc.deleting
			got := pgDerive(pgSpec("a", "b"), observed, snapshotv1alpha1.PodGroupSnapshotStatus{})
			assert.Nil(t, meta.FindStatusCondition(got.Conditions, snapshotv1alpha1.PodGroupConditionFailed))
			assert.Nil(t, meta.FindStatusCondition(got.Conditions, snapshotv1alpha1.PodGroupConditionReady))
		})
	}
}

// TestDerivePodGroupSnapshotRefusedIsCertain covers the race where a member
// fails before any member is seen to start: the group waits for every member
// to be final before it records Refused, so a peer's late Started makes it
// MemberFailed instead.
func TestDerivePodGroupSnapshotRefusedIsCertain(t *testing.T) {
	spec := pgSpec("a", "b")
	first := pgDerive(spec, pgObserved(pgAdmittedMember("a"), pgFailedMember("b", "SourcePodGone")),
		snapshotv1alpha1.PodGroupSnapshotStatus{})
	require.Equal(t, snapshotv1alpha1.CheckpointPhaseFailed, first.Members[1].Phase)
	require.Nil(t, meta.FindStatusCondition(first.Conditions, snapshotv1alpha1.PodGroupConditionFailed),
		"not Refused while member a may still start")
	admitted := meta.FindStatusCondition(first.Conditions, snapshotv1alpha1.PodGroupConditionAdmitted)
	require.NotNil(t, admitted)
	assert.Equal(t, snapshotv1alpha1.PodGroupReasonAdmitting, admitted.Reason)

	t.Run("a peer's Started arrives: MemberFailed", func(t *testing.T) {
		got := pgDerive(spec, pgObserved(pgStartedMember("a"), pgFailedMember("b", "SourcePodGone")), first)
		failed := meta.FindStatusCondition(got.Conditions, snapshotv1alpha1.PodGroupConditionFailed)
		require.NotNil(t, failed)
		assert.Equal(t, snapshotv1alpha1.PodGroupReasonMemberFailed, failed.Reason)
		assert.Equal(t, `member "b" (rank 1) failed with SourcePodGone: SourcePodGone message`, failed.Message)
		assert.True(t, meta.IsStatusConditionTrue(got.Conditions, snapshotv1alpha1.PodGroupConditionAdmitted))
	})

	t.Run("the peer then reports the session's Refused: Refused, naming b", func(t *testing.T) {
		got := pgDerive(spec, pgObserved(pgFailedMember("a", "Refused"), pgFailedMember("b", "SourcePodGone")), first)
		failed := meta.FindStatusCondition(got.Conditions, snapshotv1alpha1.PodGroupConditionFailed)
		require.NotNil(t, failed)
		assert.Equal(t, snapshotv1alpha1.PodGroupReasonRefused, failed.Reason)
		assert.Equal(t, `member "b" (rank 1) failed with SourcePodGone: SourcePodGone message`, failed.Message)
	})
}

func TestDerivePodGroupSnapshotDeadline(t *testing.T) {
	spec := pgSpec("a", "b", "c")
	observed := pgObserved(
		pgCheckpointed("a"),
		podGroupMemberObservation{podSnapshot: pgSnap("b"), content: pgContent("b", pgStarted)},
		podGroupMemberObservation{podSnapshot: pgSnap("c")},
	)
	observed.deadline = pgNow

	got := pgDerive(spec, observed, snapshotv1alpha1.PodGroupSnapshotStatus{})

	assert.Equal(t, pgPinned("a", 0, snapshotv1alpha1.CheckpointPhaseCheckpointed), withNode(got.Members[0], "node-a"))
	for _, m := range got.Members[1:] {
		assert.Equal(t, snapshotv1alpha1.CheckpointPhaseFailed, m.Phase, m.ID)
		assert.Equal(t, snapshotv1alpha1.PodGroupReasonDeadlineExceeded, m.Reason, m.ID)
	}
	assert.NotNil(t, got.Members[1].StartedAt, "startedAt survives the deadline")
	failed := meta.FindStatusCondition(got.Conditions, snapshotv1alpha1.PodGroupConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, snapshotv1alpha1.PodGroupReasonDeadlineExceeded, failed.Reason)

	// A later member failure changes neither the members nor the group.
	observed.members[1].podSnapshot = pgSnap("b", pgFailed("CriuDumpFailed"))
	again := pgDerive(spec, observed, got)
	assert.Equal(t, got, again)
}

// withNode sets the node a test observation reports, for comparing with pgPinned.
func withNode(m snapshotv1alpha1.PodGroupSnapshotMemberStatus, node string) snapshotv1alpha1.PodGroupSnapshotMemberStatus {
	m.NodeName = node
	return m
}

func TestDerivePodGroupSnapshotDeadlineAfterMemberFailure(t *testing.T) {
	spec := pgSpec("a", "b")
	observed := pgObserved(
		podGroupMemberObservation{podSnapshot: pgSnap("a", pgFailed("CriuDumpFailed")), content: pgContent("a", pgStarted)},
		podGroupMemberObservation{podSnapshot: pgSnap("b"), content: pgContent("b", pgStarted)},
	)
	first := pgDerive(spec, observed, snapshotv1alpha1.PodGroupSnapshotStatus{})
	require.Equal(t, snapshotv1alpha1.CheckpointPhaseCheckpointing, first.Members[1].Phase,
		"the operator does not fail a running member because a peer failed")

	// b's agent never reports; the deadline ends it, but the group keeps its first cause.
	observed.deadline = pgNow
	got := pgDerive(spec, observed, first)
	assert.Equal(t, snapshotv1alpha1.CheckpointPhaseFailed, got.Members[1].Phase)
	assert.Equal(t, snapshotv1alpha1.PodGroupReasonDeadlineExceeded, got.Members[1].Reason)
	failed := meta.FindStatusCondition(got.Conditions, snapshotv1alpha1.PodGroupConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, snapshotv1alpha1.PodGroupReasonMemberFailed, failed.Reason)
}

func TestDerivePodGroupSnapshotReady(t *testing.T) {
	spec := pgSpec("a", "b")
	ready := pgDerive(spec, pgObserved(pgCheckpointed("a"), pgCheckpointed("b")), snapshotv1alpha1.PodGroupSnapshotStatus{})
	cond := meta.FindStatusCondition(ready.Conditions, snapshotv1alpha1.PodGroupConditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "Checkpointed", cond.Reason)
	assert.Equal(t, "all 2 members are checkpointed", cond.Message)
	assert.Nil(t, meta.FindStatusCondition(ready.Conditions, snapshotv1alpha1.PodGroupConditionFailed))

	t.Run("not failed by a deadline that passes after it", func(t *testing.T) {
		observed := pgObserved(pgCheckpointed("a"), pgCheckpointed("b"))
		observed.deadline = pgNow
		got := pgDerive(spec, observed, ready)
		assert.Equal(t, ready, got)
	})

	for _, tc := range []struct {
		name    string
		member  podGroupMemberObservation
		message string
	}{
		{"the PodSnapshot is gone", podGroupMemberObservation{}, `member "b" (rank 1) cannot be restored: its PodSnapshot is gone`},
		{
			"the PodSnapshot was replaced",
			func() podGroupMemberObservation {
				o := pgCheckpointed("b")
				o.podSnapshot.UID = "recreated"
				return o
			}(),
			`member "b" (rank 1) cannot be restored: its PodSnapshot is gone`,
		},
		{
			"the PodSnapshot is not Ready",
			podGroupMemberObservation{podSnapshot: pgSnap("b"), content: pgContent("b", pgReady)},
			`member "b" (rank 1) cannot be restored: PodSnapshot "group-b" is not Ready`,
		},
		{
			"the content is gone",
			podGroupMemberObservation{podSnapshot: pgSnap("b", pgReady)},
			`member "b" (rank 1) cannot be restored: its PodSnapshotContent is gone`,
		},
		{
			"the content is not Ready",
			podGroupMemberObservation{podSnapshot: pgSnap("b", pgReady), content: pgContent("b", pgStarted)},
			`member "b" (rank 1) cannot be restored: PodSnapshotContent "content-b" is not Ready`,
		},
	} {
		t.Run("withdrawn when "+tc.name, func(t *testing.T) {
			got := pgDerive(spec, pgObserved(pgCheckpointed("a"), tc.member), ready)

			cond := meta.FindStatusCondition(got.Conditions, snapshotv1alpha1.PodGroupConditionReady)
			require.NotNil(t, cond)
			assert.Equal(t, metav1.ConditionFalse, cond.Status)
			assert.Equal(t, snapshotv1alpha1.PodGroupReasonArtifactUnavailable, cond.Reason)
			assert.Equal(t, tc.message, cond.Message)
			assert.Equal(t, snapshotv1alpha1.CheckpointPhaseCheckpointed, got.Members[1].Phase, "never recaptured")
			assert.Nil(t, meta.FindStatusCondition(got.Conditions, snapshotv1alpha1.PodGroupConditionFailed))
		})
	}
}

func TestDerivePodGroupSnapshotIsStable(t *testing.T) {
	spec := pgSpec("a", "b")
	observed := pgObserved(pgCheckpointed("a"), podGroupMemberObservation{podSnapshot: pgSnap("b"), content: pgContent("b", pgStarted)})
	first := pgDerive(spec, observed, snapshotv1alpha1.PodGroupSnapshotStatus{})
	second := derivePodGroupSnapshotStatus(spec, observed, first, pgNow.Add(time.Minute))
	assert.Equal(t, first, second, "deriving again from the same objects changes nothing, timestamps included")
}

func TestDerivePodGroupSnapshotCapsMessages(t *testing.T) {
	long := strings.Repeat("é", maxConditionMessageLength) // two bytes each
	snap := pgSnap("a", metav1.Condition{Type: snapshotv1alpha1.PodSnapshotConditionFailed, Status: metav1.ConditionTrue,
		Reason: "Long", Message: long})
	got := pgDerive(pgSpec("a"), pgObserved(podGroupMemberObservation{podSnapshot: snap}), snapshotv1alpha1.PodGroupSnapshotStatus{})

	assert.LessOrEqual(t, len(got.Members[0].Message), maxConditionMessageLength)
	for _, cond := range got.Conditions {
		assert.LessOrEqual(t, len(cond.Message), maxConditionMessageLength, cond.Type)
		assert.True(t, strings.HasPrefix(cond.Message, `member "a" (rank 0) failed with Long: é`), cond.Type)
	}
}

func TestPodGroupSnapshotReleasable(t *testing.T) {
	failed := metav1.Condition{Type: snapshotv1alpha1.PodGroupConditionFailed, Status: metav1.ConditionTrue, Reason: "MemberFailed"}
	ready := metav1.Condition{Type: snapshotv1alpha1.PodGroupConditionReady, Status: metav1.ConditionTrue, Reason: "Checkpointed"}
	withdrawn := metav1.Condition{Type: snapshotv1alpha1.PodGroupConditionReady, Status: metav1.ConditionFalse,
		Reason: snapshotv1alpha1.PodGroupReasonArtifactUnavailable}
	for _, tc := range []struct {
		name       string
		conditions []metav1.Condition
		phases     []snapshotv1alpha1.CheckpointPhase
		want       bool
	}{
		{"running", nil, []snapshotv1alpha1.CheckpointPhase{"Checkpointing", "Admitted"}, false},
		{"cancelled", []metav1.Condition{failed}, []snapshotv1alpha1.CheckpointPhase{"Failed", "Failed"}, true},
		{"failed while a member still dumps", []metav1.Condition{failed}, []snapshotv1alpha1.CheckpointPhase{"Failed", "Checkpointing"}, false},
		{"failed with a member never started", []metav1.Condition{failed}, []snapshotv1alpha1.CheckpointPhase{"Failed", "Admitted"}, true},
		{"ready", []metav1.Condition{ready}, []snapshotv1alpha1.CheckpointPhase{"Checkpointed", "Checkpointed"}, true},
		{"ready withdrawn", []metav1.Condition{withdrawn}, []snapshotv1alpha1.CheckpointPhase{"Checkpointed", "Checkpointed"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := &snapshotv1alpha1.PodGroupSnapshotStatus{Conditions: tc.conditions}
			for _, phase := range tc.phases {
				status.Members = append(status.Members, snapshotv1alpha1.PodGroupSnapshotMemberStatus{Phase: phase})
			}
			assert.Equal(t, tc.want, podGroupSnapshotReleasable(status))
		})
	}
}

func TestPodGroupSnapshotTransitions(t *testing.T) {
	cond := func(condType string, status metav1.ConditionStatus, reason string) metav1.Condition {
		return metav1.Condition{Type: condType, Status: status, Reason: reason, Message: reason + " message"}
	}
	admitting := cond(snapshotv1alpha1.PodGroupConditionAdmitted, metav1.ConditionFalse, "Admitting")
	committed := cond(snapshotv1alpha1.PodGroupConditionAdmitted, metav1.ConditionTrue, "Committed")
	ready := cond(snapshotv1alpha1.PodGroupConditionReady, metav1.ConditionTrue, "Checkpointed")
	withdrawn := cond(snapshotv1alpha1.PodGroupConditionReady, metav1.ConditionFalse, "ArtifactUnavailable")
	failed := cond(snapshotv1alpha1.PodGroupConditionFailed, metav1.ConditionTrue, "MemberFailed")

	for _, tc := range []struct {
		name           string
		previous, next []metav1.Condition
		want           []podGroupEvent
	}{
		{"nothing changes", []metav1.Condition{committed}, []metav1.Condition{committed}, nil},
		{"waiting changes nothing", nil, []metav1.Condition{admitting}, nil},
		{"admitted", []metav1.Condition{admitting}, []metav1.Condition{committed},
			[]podGroupEvent{{corev1.EventTypeNormal, "Committed", "Committed message"}}},
		{"ready", []metav1.Condition{committed}, []metav1.Condition{committed, ready},
			[]podGroupEvent{{corev1.EventTypeNormal, "Checkpointed", "Checkpointed message"}}},
		{"failed", []metav1.Condition{committed}, []metav1.Condition{committed, failed},
			[]podGroupEvent{{corev1.EventTypeWarning, "MemberFailed", "MemberFailed message"}}},
		{"ready withdrawn", []metav1.Condition{committed, ready}, []metav1.Condition{committed, withdrawn},
			[]podGroupEvent{{corev1.EventTypeWarning, "ArtifactUnavailable", "ArtifactUnavailable message"}}},
		{"never ready is not withdrawn", []metav1.Condition{committed}, []metav1.Condition{committed, withdrawn}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, podGroupSnapshotTransitions(tc.previous, tc.next))
		})
	}
}

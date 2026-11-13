// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// podGroupMemberObservation is what one reconcile saw of one member.
type podGroupMemberObservation struct {
	// podSnapshot is the member's PodSnapshot: the one at the member's name
	// that this group controls and whose source is the member's. Nil when
	// there is none.
	podSnapshot *snapshotv1alpha1.PodSnapshot
	// content is the PodSnapshotContent bound to podSnapshot, when it exists
	// and points back to it. Its Started condition and identity come from it.
	content *snapshotv1alpha1.PodSnapshotContent
	// nodeName is the node the member's source Pod runs on, when known.
	nodeName string
	// podSnapshotDeleted is true when the observation found the member's
	// pinned PodSnapshot gone, or replaced by another object, while the member
	// was unfinished. It fails the member for good, so Reconcile records it
	// only from an observation through the API server.
	podSnapshotDeleted bool
}

// podGroupSnapshotObservation is everything derivePodGroupSnapshotStatus reads
// besides the spec and the previous status.
type podGroupSnapshotObservation struct {
	// members holds one entry per spec member, in rank order.
	members   []podGroupMemberObservation
	admission podGroupAdmission
	// deleting is true once the group has a deletion timestamp.
	deleting bool
	// deadline is the group's creation time plus spec.deadlineSeconds.
	deadline time.Time
}

// member returns the observation of the member at rank, or an empty one.
func (o podGroupSnapshotObservation) member(rank int) podGroupMemberObservation {
	if rank < len(o.members) {
		return o.members[rank]
	}
	return podGroupMemberObservation{}
}

// The messages of a member failed by a cause of the whole group, and the
// group's message when it is deleted before any member started.
const (
	podGroupRefusedMemberMessage   = "the PodGroupSnapshot was refused; see its Failed condition"
	podGroupDeadlineMemberMessage  = "the PodGroupSnapshot's deadline passed; see its Failed condition"
	podGroupCancelledMemberMessage = "the PodGroupSnapshot was deleted before any member started; see its Failed condition"
	podGroupCancelledMessage       = "the PodGroupSnapshot was deleted before any member started"
)

// podGroupSnapshotKind is what the shared rules say for a PodGroupSnapshot.
var podGroupSnapshotKind = podGroupKind{
	hiddenStartReason:      snapshotv1alpha1.ReasonPodSnapshotDeleted,
	done:                   "checkpointed",
	refusedMemberMessage:   podGroupRefusedMemberMessage,
	deadlineMemberMessage:  podGroupDeadlineMemberMessage,
	cancelledMemberMessage: podGroupCancelledMemberMessage,
	cancelledMessage:       podGroupCancelledMessage,
	committedMessage:       "every member passed admission, and the checkpoint has started",
	admittingMessage:       "member PodSnapshots are created; waiting for every member to pass admission",
}

// derivePodGroupSnapshotStatus computes a PodGroupSnapshot's status from its
// spec, one reconcile's observations and the previous status, at time now. It
// is pure: it makes no client calls, so a restarted operator derives the same
// status from the same objects.
//
// Each member's phase comes from the first matching row:
//
//	Failed         its PodSnapshot is Failed: that condition's reason, and its
//	               message cut to maxMemberMessageLength bytes
//	Failed         its pinned PodSnapshot is gone, as the API server confirmed:
//	               PodSnapshotDeleted
//	Failed         the group was refused, passed its deadline, or was deleted
//	               before any member started, and the member is not
//	               Checkpointed: that cause's reason, with a fixed message
//	               pointing to the group's Failed condition
//	Checkpointed   its PodSnapshot is Ready and its content is seen: pins content
//	Checkpointing  it has startedAt
//	Admitted       it has a PodSnapshot: pins podSnapshot
//	Pending        otherwise
//
// Checkpointed and Failed are final, and podSnapshot, content and startedAt
// are write-once: the API server refuses to change them, so they carry over and
// are only ever filled in. startedAt is the lastTransitionTime of the content's
// Started condition, copied whenever it is seen, even on a member that is
// already Failed, since a member can fail before the operator ever sees it
// Checkpointing. Phases never move back, because Checkpointing and Admitted
// rest on write-once fields.
//
// The group's Failed and Admitted conditions follow the rules both kinds share
// (see podGroupAttempt.updateConditions), and Ready is the PodGroupSnapshot's
// own: once every member is Checkpointed, True while every member's
// PodSnapshot and content exist and are Ready, otherwise
// False/ArtifactUnavailable. It is never recaptured.
func derivePodGroupSnapshotStatus(
	spec *snapshotv1alpha1.PodGroupSnapshotSpec,
	observed podGroupSnapshotObservation,
	previous snapshotv1alpha1.PodGroupSnapshotStatus,
	now time.Time,
) snapshotv1alpha1.PodGroupSnapshotStatus {
	next := previous.DeepCopy()
	previousMembers := make(map[string]snapshotv1alpha1.PodGroupSnapshotMemberStatus, len(next.Members))
	for _, m := range next.Members {
		previousMembers[m.ID] = m
	}

	next.Members = make([]snapshotv1alpha1.PodGroupSnapshotMemberStatus, len(spec.Members))
	available := make([]memberAvailability, len(spec.Members))
	for rank, member := range spec.Members {
		status := previousMembers[member.ID]
		status.ID = member.ID
		status.Rank = int32(rank)
		if status.Phase == "" {
			status.Phase = snapshotv1alpha1.CheckpointPhasePending
		}
		available[rank] = deriveMemberStatus(&status, observed.member(rank))
		next.Members[rank] = status
	}

	attempt := newPodGroupAttempt(&podGroupSnapshotKind, spec.DeadlineSeconds, observed.admission,
		observed.deleting, !now.Before(observed.deadline), podGroupSnapshotMemberViews(next.Members))
	if cause := attempt.memberCause(); cause != nil {
		for i := range next.Members {
			if m := &next.Members[i]; !isCheckpointPhaseFinal(m.Phase) {
				m.Phase, m.Reason, m.Message = snapshotv1alpha1.CheckpointPhaseFailed, cause.reason, cause.message
			}
		}
	}

	attempt.updateConditions(&next.Conditions, now)

	if attempt.outcomes.allDone {
		if unavailable := firstUnavailableMember(next.Members, available); unavailable != "" {
			setPodGroupCondition(&next.Conditions, snapshotv1alpha1.PodGroupConditionReady, metav1.ConditionFalse,
				podGroupCause{snapshotv1alpha1.PodGroupReasonArtifactUnavailable, unavailable}, now)
		} else {
			setPodGroupCondition(&next.Conditions, snapshotv1alpha1.PodGroupConditionReady, metav1.ConditionTrue,
				podGroupCause{string(snapshotv1alpha1.CheckpointPhaseCheckpointed),
					fmt.Sprintf("all %d members are checkpointed", len(next.Members))}, now)
		}
	}
	return *next
}

// podGroupSnapshotChangeIsIrreversible reports whether recording derived over
// the persisted status of a PodGroupSnapshot would make an irreversible
// decision (see isIrreversiblePodGroupChange). deleting says whether the group
// is being deleted, so that Reconcile would release its finalizer once derived
// is recorded.
func podGroupSnapshotChangeIsIrreversible(persisted, derived *snapshotv1alpha1.PodGroupSnapshotStatus, deleting bool) bool {
	return isIrreversiblePodGroupChange(
		podGroupSnapshotMemberViews(persisted.Members), podGroupSnapshotMemberViews(derived.Members),
		persisted.Conditions, derived.Conditions,
		deleting && podGroupSnapshotReleasable(derived),
	)
}

// podGroupSnapshotMemberViews returns the shared rules' view of each member.
func podGroupSnapshotMemberViews(members []snapshotv1alpha1.PodGroupSnapshotMemberStatus) []podGroupMemberView {
	views := make([]podGroupMemberView, len(members))
	for i, m := range members {
		views[i] = podGroupMemberView{
			id:      m.ID,
			rank:    m.Rank,
			started: m.StartedAt != nil,
			hasWork: m.PodSnapshot != nil,
			done:    m.Phase == snapshotv1alpha1.CheckpointPhaseCheckpointed,
			failed:  m.Phase == snapshotv1alpha1.CheckpointPhaseFailed,
			reason:  m.Reason,
			message: m.Message,
		}
	}
	return views
}

// memberAvailability is what a Checkpointed member's artifact needs to stay
// restorable: its PodSnapshot and content, as observed after pinning.
type memberAvailability struct {
	podSnapshot *snapshotv1alpha1.PodSnapshot
	content     *snapshotv1alpha1.PodSnapshotContent
}

// deriveMemberStatus fills in a member's write-once fields from what was
// observed and moves its phase by the member table. Observations that
// contradict a pinned identity are ignored: they belong to another object.
func deriveMemberStatus(m *snapshotv1alpha1.PodGroupSnapshotMemberStatus, observed podGroupMemberObservation) memberAvailability {
	snap, content := observed.podSnapshot, observed.content
	if snap != nil && m.PodSnapshot != nil && snap.UID != m.PodSnapshot.UID {
		snap, content = nil, nil
	}
	if content != nil && m.Content != nil && content.UID != m.Content.UID {
		content = nil
	}

	if m.NodeName == "" {
		m.NodeName = observed.nodeName
	}
	if snap != nil && m.PodSnapshot == nil {
		m.PodSnapshot = &snapshotv1alpha1.ObjectIdentity{Name: snap.Name, UID: snap.UID}
	}
	if content != nil && m.StartedAt == nil {
		if started := meta.FindStatusCondition(content.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionStarted); started != nil &&
			started.Status == metav1.ConditionTrue {
			m.StartedAt = started.LastTransitionTime.DeepCopy()
		}
	}

	if isCheckpointPhaseFinal(m.Phase) {
		return memberAvailability{podSnapshot: snap, content: content}
	}
	m.Reason, m.Message = "", ""
	switch {
	case snap != nil && snapshotv1alpha1.IsPodSnapshotFailed(snap):
		cond := meta.FindStatusCondition(snap.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
		m.Phase, m.Reason = snapshotv1alpha1.CheckpointPhaseFailed, cond.Reason
		m.Message = capMessage(cond.Message, maxMemberMessageLength)
	case observed.podSnapshotDeleted && m.PodSnapshot != nil:
		m.Phase, m.Reason = snapshotv1alpha1.CheckpointPhaseFailed, snapshotv1alpha1.ReasonPodSnapshotDeleted
		m.Message = fmt.Sprintf("PodSnapshot %q was deleted before the member was checkpointed", m.PodSnapshot.Name)
	case snap != nil && snapshotv1alpha1.IsPodSnapshotSucceeded(snap) && content != nil:
		m.Phase = snapshotv1alpha1.CheckpointPhaseCheckpointed
		m.Content = &snapshotv1alpha1.ObjectIdentity{Name: content.Name, UID: content.UID}
	case m.StartedAt != nil:
		m.Phase = snapshotv1alpha1.CheckpointPhaseCheckpointing
	case m.PodSnapshot != nil:
		m.Phase = snapshotv1alpha1.CheckpointPhaseAdmitted
	default:
		m.Phase = snapshotv1alpha1.CheckpointPhasePending
	}
	return memberAvailability{podSnapshot: snap, content: content}
}

// firstUnavailableMember describes the first member, in rank order, whose
// PodSnapshot or content is gone or not Ready, or returns "" when every member
// can be restored.
func firstUnavailableMember(members []snapshotv1alpha1.PodGroupSnapshotMemberStatus, available []memberAvailability) string {
	for i, m := range members {
		snap, content := available[i].podSnapshot, available[i].content
		var problem string
		switch {
		case snap == nil:
			problem = "its PodSnapshot is gone"
		case !snapshotv1alpha1.IsPodSnapshotSucceeded(snap):
			problem = fmt.Sprintf("PodSnapshot %q is not Ready", snap.Name)
		case content == nil:
			problem = "its PodSnapshotContent is gone"
		case !snapshotv1alpha1.IsPodSnapshotContentSucceeded(content):
			problem = fmt.Sprintf("PodSnapshotContent %q is not Ready", content.Name)
		default:
			continue
		}
		return fmt.Sprintf("member %q (rank %d) cannot be restored: %s", m.ID, m.Rank, problem)
	}
	return ""
}

// isCheckpointPhaseFinal reports whether a member's phase can no longer change.
func isCheckpointPhaseFinal(phase snapshotv1alpha1.CheckpointPhase) bool {
	return phase == snapshotv1alpha1.CheckpointPhaseCheckpointed || phase == snapshotv1alpha1.CheckpointPhaseFailed
}

// podGroupSnapshotReleasable reports whether a PodGroupSnapshot that is being
// deleted may drop its finalizer: once its attempt has ended, Ready or Failed,
// and no member is still being checkpointed, so garbage collection never
// deletes a member's PodSnapshot while its agent is dumping. A group deleted
// before any member started has ended with Cancelled. The deadline bounds the
// wait: it turns every member still Checkpointing into Failed, and the group
// into Failed if it was not already.
func podGroupSnapshotReleasable(status *snapshotv1alpha1.PodGroupSnapshotStatus) bool {
	ended := meta.FindStatusCondition(status.Conditions, snapshotv1alpha1.PodGroupConditionReady) != nil ||
		meta.IsStatusConditionTrue(status.Conditions, snapshotv1alpha1.PodGroupConditionFailed)
	if !ended {
		return false
	}
	for _, m := range status.Members {
		if m.Phase == snapshotv1alpha1.CheckpointPhaseCheckpointing {
			return false
		}
	}
	return true
}

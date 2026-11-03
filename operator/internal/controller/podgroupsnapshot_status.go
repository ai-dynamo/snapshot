// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// podGroupAdmissionState is how far a PodGroupSnapshot's admission got in one
// reconcile.
type podGroupAdmissionState int

const (
	// The zero value means admission did not run: member work already exists,
	// or the group has failed, is being deleted, or is past its deadline.
	_ podGroupAdmissionState = iota
	// podGroupAdmissionWaiting means a source Pod is not scheduled yet, or the
	// agent on a member's node is not ready for groups.
	podGroupAdmissionWaiting
	// podGroupAdmissionRefused means the group can never pass admission.
	podGroupAdmissionRefused
	// podGroupAdmissionPassed means the operator may create every member's work.
	podGroupAdmissionPassed
)

// podGroupAdmission is the outcome of admission. The reason and message explain
// a wait or a refusal.
type podGroupAdmission struct {
	state   podGroupAdmissionState
	reason  string
	message string
}

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
	// podSnapshotDeleted is true when the API server confirmed that the
	// member's pinned PodSnapshot is gone, or replaced by another object.
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

// maxConditionMessageLength is the API server's limit on a condition's message
// and on a member's message.
const maxConditionMessageLength = 32768

// podGroupCause is why the operator fails a group or a member: a reason and its
// message.
type podGroupCause struct {
	reason  string
	message string
}

// derivePodGroupSnapshotStatus computes a PodGroupSnapshot's status from its
// spec, one reconcile's observations and the previous status, at time now. It
// is pure: it makes no client calls, so a restarted operator derives the same
// status from the same objects.
//
// Each member's phase comes from the first matching row:
//
//	Failed         its PodSnapshot is Failed: that condition's reason and message
//	Failed         its pinned PodSnapshot is gone, as the API server confirmed:
//	               PodSnapshotDeleted
//	Failed         the group was refused, passed its deadline, or was deleted
//	               before any member started, and the member is not
//	               Checkpointed: that cause's reason
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
// The group's conditions:
//
//	Admitted  False while admission waits, with its reason; False/Admitting once
//	          member work exists; True/Committed once any member has startedAt,
//	          since agents set Started only after the admitted barrier; False
//	          with the failure's reason if the group fails before that.
//	Ready     once every member is Checkpointed: True while every member's
//	          PodSnapshot and content exist and are Ready, otherwise
//	          False/ArtifactUnavailable. Never recaptured.
//	Failed    True with the first cause, never rewritten afterwards; see
//	          podGroupFailure.
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

	// What the members reported, before the operator's causes fail the
	// unfinished ones.
	outcomes := podGroupMemberOutcomesOf(next.Members)

	pastDeadline := !now.Before(observed.deadline)
	if cause := podGroupMemberCause(spec, observed, pastDeadline, outcomes.committed); cause != nil {
		for i := range next.Members {
			if m := &next.Members[i]; !isCheckpointPhaseFinal(m.Phase) {
				m.Phase, m.Reason, m.Message = snapshotv1alpha1.CheckpointPhaseFailed, cause.reason, capMessage(cause.message)
			}
		}
	}

	allCheckpointed := true
	for _, m := range next.Members {
		allCheckpointed = allCheckpointed && m.Phase == snapshotv1alpha1.CheckpointPhaseCheckpointed
	}

	failed := meta.IsStatusConditionTrue(next.Conditions, snapshotv1alpha1.PodGroupConditionFailed)
	if !failed && !allCheckpointed {
		if cause := podGroupFailure(spec, observed, next, pastDeadline, outcomes); cause != nil {
			setPodGroupCondition(next, snapshotv1alpha1.PodGroupConditionFailed, metav1.ConditionTrue, *cause, now)
			failed = true
		}
	}

	if allCheckpointed {
		if unavailable := firstUnavailableMember(next.Members, available); unavailable != "" {
			setPodGroupCondition(next, snapshotv1alpha1.PodGroupConditionReady, metav1.ConditionFalse,
				podGroupCause{snapshotv1alpha1.PodGroupReasonArtifactUnavailable, unavailable}, now)
		} else {
			setPodGroupCondition(next, snapshotv1alpha1.PodGroupConditionReady, metav1.ConditionTrue,
				podGroupCause{string(snapshotv1alpha1.CheckpointPhaseCheckpointed),
					fmt.Sprintf("all %d members are checkpointed", len(next.Members))}, now)
		}
	}

	deriveAdmittedCondition(next, observed.admission, outcomes.committed, failed, now)
	return *next
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
		m.Phase, m.Reason, m.Message = snapshotv1alpha1.CheckpointPhaseFailed, cond.Reason, capMessage(cond.Message)
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

// podGroupMemberCause is the cause that fails every unfinished member: the
// three ways the operator itself ends an attempt. It does not depend on the
// group's Failed reason, so a group that failed because of one member still
// fails the others at its deadline.
func podGroupMemberCause(
	spec *snapshotv1alpha1.PodGroupSnapshotSpec,
	observed podGroupSnapshotObservation,
	pastDeadline, committed bool,
) *podGroupCause {
	switch {
	case observed.admission.state == podGroupAdmissionRefused:
		return &podGroupCause{observed.admission.reason, observed.admission.message}
	case pastDeadline:
		return &podGroupCause{snapshotv1alpha1.PodGroupReasonDeadlineExceeded,
			fmt.Sprintf("the deadline of %d seconds passed before the member was checkpointed", spec.DeadlineSeconds)}
	case observed.deleting && !committed:
		return &podGroupCause{snapshotv1alpha1.PodGroupReasonCancelled, podGroupCancelledMessage}
	}
	return nil
}

const podGroupCancelledMessage = "the PodGroupSnapshot was deleted before any member started"

// podGroupMemberOutcomes is what the members reported, taken before the
// deadline or a cancellation fails the unfinished ones.
type podGroupMemberOutcomes struct {
	// committed is true when some member has startedAt.
	committed bool
	// allFinal is true when every member is Checkpointed or Failed.
	allFinal bool
	// reported is true when every member is final and none failed with
	// PodSnapshotDeleted: that member's content is gone, so a Started it
	// recorded may never have been seen.
	reported bool
	// named is the Failed member that a group failure names (see
	// nameFailedMember), or nil when no member failed.
	named *snapshotv1alpha1.PodGroupSnapshotMemberStatus
	// ownFailure is the same among the members whose own PodSnapshot reported
	// the failure, which leaves out PodSnapshotDeleted.
	ownFailure *snapshotv1alpha1.PodGroupSnapshotMemberStatus
}

// podGroupMemberOutcomesOf takes the outcomes from the members' statuses, in
// rank order. Call it before the deadline or a cancellation fails the
// unfinished members; the named members point into members.
func podGroupMemberOutcomesOf(members []snapshotv1alpha1.PodGroupSnapshotMemberStatus) podGroupMemberOutcomes {
	o := podGroupMemberOutcomes{allFinal: true, reported: true}
	for i := range members {
		m := &members[i]
		o.committed = o.committed || m.StartedAt != nil
		if !isCheckpointPhaseFinal(m.Phase) {
			o.allFinal, o.reported = false, false
		}
		if m.Phase != snapshotv1alpha1.CheckpointPhaseFailed {
			continue
		}
		o.named = nameFailedMember(o.named, m)
		if m.Reason == snapshotv1alpha1.ReasonPodSnapshotDeleted {
			o.reported = false
		} else {
			o.ownFailure = nameFailedMember(o.ownFailure, m)
		}
	}
	return o
}

// nameFailedMember returns which of two Failed members, named before m in rank
// order, a group failure names. A member with a cause of its own wins over one
// with a group reason, which only says how the group's session ended for it;
// otherwise the first in rank order wins.
func nameFailedMember(named, m *snapshotv1alpha1.PodGroupSnapshotMemberStatus) *snapshotv1alpha1.PodGroupSnapshotMemberStatus {
	if named == nil || (isPodGroupSessionReason(named.Reason) && !isPodGroupSessionReason(m.Reason)) {
		return m
	}
	return named
}

// isPodGroupSessionReason reports whether a member's reason is one of the group
// reasons, which every member of a session that ended reports alike.
func isPodGroupSessionReason(reason string) bool {
	switch reason {
	case snapshotv1alpha1.PodGroupReasonRefused,
		snapshotv1alpha1.PodGroupReasonMemberFailed,
		snapshotv1alpha1.PodGroupReasonCoordinatorLost,
		snapshotv1alpha1.PodGroupReasonDeadlineExceeded,
		snapshotv1alpha1.PodGroupReasonCancelled:
		return true
	}
	return false
}

// podGroupFailure is the group's failure cause, for a group that has neither
// failed nor checkpointed every member, or nil while it has none. outcomes is
// taken before the deadline or a cancellation fails the unfinished members,
// and the first matching row wins:
//
//	1  admission refused                      Refused
//	2  committed, and some member Failed,     the named member's reason if
//	   PodSnapshotDeleted included            CoordinatorLost, DeadlineExceeded or
//	                                          Cancelled, otherwise MemberFailed
//	3  not committed, and reported            Refused
//	4  the deadline passed                    DeadlineExceeded
//	5  deleted, and not committed             Cancelled
//	6  every member final, some member Failed MemberFailed
//
// Rows 2, 3 and 6 name the named member, with its reason and message. Row 4
// adds the member that had already failed on its own, if any. Refused is
// certain: it needs every member to have reported, so none can still start,
// and none with a deleted PodSnapshot, whose Started may never have been seen.
// A finished attempt never waits for its deadline: once every member is final,
// row 2, 3, 5 or 6 applies.
func podGroupFailure(
	spec *snapshotv1alpha1.PodGroupSnapshotSpec,
	observed podGroupSnapshotObservation,
	next *snapshotv1alpha1.PodGroupSnapshotStatus,
	pastDeadline bool,
	outcomes podGroupMemberOutcomes,
) *podGroupCause {
	named := outcomes.named
	switch {
	case observed.admission.state == podGroupAdmissionRefused:
		return &podGroupCause{observed.admission.reason, observed.admission.message}
	case outcomes.committed && named != nil:
		reason := snapshotv1alpha1.PodGroupReasonMemberFailed
		switch named.Reason {
		case snapshotv1alpha1.PodGroupReasonCoordinatorLost,
			snapshotv1alpha1.PodGroupReasonDeadlineExceeded,
			snapshotv1alpha1.PodGroupReasonCancelled:
			reason = named.Reason
		}
		return &podGroupCause{reason, podGroupMemberFailureMessage(named)}
	case !outcomes.committed && outcomes.reported && named != nil:
		return &podGroupCause{snapshotv1alpha1.PodGroupReasonRefused, podGroupMemberFailureMessage(named)}
	case pastDeadline:
		message := podGroupDeadlineMessage(spec, next)
		if outcomes.ownFailure != nil {
			message += "; " + podGroupMemberFailureMessage(outcomes.ownFailure)
		}
		return &podGroupCause{snapshotv1alpha1.PodGroupReasonDeadlineExceeded, message}
	case observed.deleting && !outcomes.committed:
		return &podGroupCause{snapshotv1alpha1.PodGroupReasonCancelled, podGroupCancelledMessage}
	case outcomes.allFinal && named != nil:
		return &podGroupCause{snapshotv1alpha1.PodGroupReasonMemberFailed, podGroupMemberFailureMessage(named)}
	}
	return nil
}

// podGroupMemberFailureMessage names a Failed member, with its reason and
// message.
func podGroupMemberFailureMessage(m *snapshotv1alpha1.PodGroupSnapshotMemberStatus) string {
	return fmt.Sprintf("member %q (rank %d) failed with %s: %s", m.ID, m.Rank, m.Reason, m.Message)
}

// podGroupDeadlineMessage says how far the group got by its deadline. A group
// that never got past admission says what admission was waiting for, which is
// the usual reason a deadline expires that early.
func podGroupDeadlineMessage(spec *snapshotv1alpha1.PodGroupSnapshotSpec, next *snapshotv1alpha1.PodGroupSnapshotStatus) string {
	if admitted := meta.FindStatusCondition(next.Conditions, snapshotv1alpha1.PodGroupConditionAdmitted); admitted != nil &&
		admitted.Status == metav1.ConditionFalse && isPodGroupAdmissionWaitReason(admitted.Reason) {
		return fmt.Sprintf("the deadline of %d seconds passed while admission was waiting: %s",
			spec.DeadlineSeconds, admitted.Message)
	}
	unfinished := 0
	for _, m := range next.Members {
		if m.Phase != snapshotv1alpha1.CheckpointPhaseCheckpointed {
			unfinished++
		}
	}
	return fmt.Sprintf("the deadline of %d seconds passed with %d of %d members not checkpointed",
		spec.DeadlineSeconds, unfinished, len(next.Members))
}

// deriveAdmittedCondition sets the Admitted condition. Committed is permanent,
// because startedAt is write-once.
func deriveAdmittedCondition(
	next *snapshotv1alpha1.PodGroupSnapshotStatus,
	admission podGroupAdmission,
	committed, failed bool,
	now time.Time,
) {
	memberWorkExists := admission.state == podGroupAdmissionPassed
	for _, m := range next.Members {
		memberWorkExists = memberWorkExists || m.PodSnapshot != nil
	}
	var status metav1.ConditionStatus
	var cause podGroupCause
	switch {
	case committed:
		status, cause = metav1.ConditionTrue, podGroupCause{snapshotv1alpha1.PodGroupReasonCommitted,
			"every member passed admission, and the checkpoint has started"}
	case failed:
		failure := meta.FindStatusCondition(next.Conditions, snapshotv1alpha1.PodGroupConditionFailed)
		status, cause = metav1.ConditionFalse, podGroupCause{failure.Reason, failure.Message}
	case admission.state == podGroupAdmissionWaiting:
		status, cause = metav1.ConditionFalse, podGroupCause{admission.reason, admission.message}
	case memberWorkExists:
		status, cause = metav1.ConditionFalse, podGroupCause{snapshotv1alpha1.PodGroupReasonAdmitting,
			"member PodSnapshots are created; waiting for every member to pass admission"}
	default:
		return
	}
	setPodGroupCondition(next, snapshotv1alpha1.PodGroupConditionAdmitted, status, cause, now)
}

// isPodGroupAdmissionWaitReason reports whether reason is one that admission
// waits with.
func isPodGroupAdmissionWaitReason(reason string) bool {
	switch reason {
	case snapshotv1alpha1.PodGroupReasonPreviousGroupCleanup,
		snapshotv1alpha1.PodGroupReasonSourcePodUnscheduled,
		snapshotv1alpha1.PodGroupReasonAgentNotReady:
		return true
	}
	return false
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

// podGroupEvent is an event for one transition of a group's conditions.
type podGroupEvent struct {
	eventType string
	reason    string
	message   string
}

// podGroupSnapshotTransitions lists the events for the transitions from
// previous to next: Admitted, Ready and Failed turning True, and Ready turning
// False after it was True. Each event carries the condition's reason and
// message, so a reconcile that changes nothing emits nothing.
func podGroupSnapshotTransitions(previous, next []metav1.Condition) []podGroupEvent {
	var events []podGroupEvent
	for _, t := range []struct {
		condType  string
		eventType string
	}{
		{snapshotv1alpha1.PodGroupConditionAdmitted, corev1.EventTypeNormal},
		{snapshotv1alpha1.PodGroupConditionReady, corev1.EventTypeNormal},
		{snapshotv1alpha1.PodGroupConditionFailed, corev1.EventTypeWarning},
	} {
		cond := meta.FindStatusCondition(next, t.condType)
		if cond == nil {
			continue
		}
		was := meta.IsStatusConditionTrue(previous, t.condType)
		switch {
		case !was && cond.Status == metav1.ConditionTrue:
			events = append(events, podGroupEvent{t.eventType, cond.Reason, cond.Message})
		case was && cond.Status != metav1.ConditionTrue && t.condType == snapshotv1alpha1.PodGroupConditionReady:
			events = append(events, podGroupEvent{corev1.EventTypeWarning, cond.Reason, cond.Message})
		}
	}
	return events
}

// setPodGroupCondition sets a condition on a group's status, with its message
// cut to the API server's limit.
func setPodGroupCondition(
	status *snapshotv1alpha1.PodGroupSnapshotStatus,
	condType string,
	conditionStatus metav1.ConditionStatus,
	cause podGroupCause,
	now time.Time,
) {
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             conditionStatus,
		Reason:             cause.reason,
		Message:            capMessage(cause.message),
		LastTransitionTime: metav1.NewTime(now),
	})
}

// capMessage cuts a message to maxConditionMessageLength bytes, dropping any
// character the cut splits. A member's message can already be at the limit,
// and the group's message adds to it.
func capMessage(message string) string {
	if len(message) <= maxConditionMessageLength {
		return message
	}
	return strings.ToValidUTF8(message[:maxConditionMessageLength], "")
}

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

// The status rules that both kinds of pod group share. SNEP-324 gives both
// kinds one rule set, so some of these rules only a PodGroupRestore exercises.
// Each kind keeps its own member table and observations, turns its members'
// statuses into podGroupMemberViews, and applies these rules to them through a
// podGroupAttempt.

// podGroupAdmissionState is how far a group's admission got in one reconcile.
// The zero value means admission did not run: member work already exists, or
// the group has failed, is being deleted, or is past its deadline.
type podGroupAdmissionState int

const (
	// podGroupAdmissionWaiting means admission waits for something that will
	// come, such as a Pod to be scheduled or the agent on a member's node.
	podGroupAdmissionWaiting podGroupAdmissionState = iota + 1
	// podGroupAdmissionRefused means the group can never pass admission.
	podGroupAdmissionRefused
	// podGroupAdmissionPassed means the operator may start every member's work.
	podGroupAdmissionPassed
)

// podGroupAdmission is the outcome of admission. The reason and message explain
// a wait or a refusal.
type podGroupAdmission struct {
	state   podGroupAdmissionState
	reason  string
	message string
}

// podGroupRefusal refuses a group with Refused and message.
func podGroupRefusal(message string) podGroupAdmission {
	return podGroupAdmission{podGroupAdmissionRefused, snapshotv1alpha1.PodGroupReasonRefused, message}
}

const (
	// maxConditionMessageLength is the API server's limit on a condition's
	// message.
	maxConditionMessageLength = 32768
	// maxMemberMessageLength is the API server's limit on a member's message:
	// short, so that the messages of 256 members fit in one object.
	maxMemberMessageLength = 1024
)

// podGroupCause is why the operator fails a group or a member: a reason and its
// message.
type podGroupCause struct {
	reason  string
	message string
}

// podGroupKind holds what the shared rules say differently for each kind of
// group.
type podGroupKind struct {
	// hiddenStartReason is the reason of a member whose record of its start
	// may have gone with a deleted object, so it never counts as reported.
	hiddenStartReason string
	// done says, in messages, what a member that succeeded is.
	done string
	// The fixed messages of a member failed by a cause of the whole group (see
	// memberCause). Each is short and fixed, so a long cause is never copied
	// into every member; the group's Failed condition keeps the detail.
	refusedMemberMessage, deadlineMemberMessage, cancelledMemberMessage string
	// cancelledMessage is the group's Failed message when it is deleted
	// before any member started.
	cancelledMessage string
	// committedMessage and admittingMessage explain the Admitted condition
	// once the attempt is committed, and while member work exists before that.
	committedMessage, admittingMessage string
}

// podGroupMemberView is what the shared rules read of one member's status.
type podGroupMemberView struct {
	id   string
	rank int32
	// started is true when the member has startedAt.
	started bool
	// hasWork is true once the member's work exists: its PodSnapshot is
	// created, or its restore Pod is pinned.
	hasWork bool
	// done is true when the member succeeded, and failed when it failed; both
	// phases are final.
	done, failed    bool
	reason, message string
}

// isIrreversiblePodGroupChange reports whether recording a derived status over
// the persisted one would make a decision that can never be undone, so that
// the controller observes again through the API server before it records
// anything:
//
//   - a member's phase becomes final;
//   - the group's Failed or Ready condition turns True;
//   - or, for a group being deleted, the reconcile would release the
//     finalizer, which releases says.
//
// It does not tell an outcome the operator imposes from one a member
// reported: confirming a reported one costs a few reads and is harmless.
// Members are matched by ID, so a member the persisted status lacks counts as
// unfinished.
func isIrreversiblePodGroupChange(
	persistedMembers, derivedMembers []podGroupMemberView,
	persistedConditions, derivedConditions []metav1.Condition,
	releases bool,
) bool {
	if releases {
		return true
	}
	for _, condType := range []string{snapshotv1alpha1.PodGroupConditionFailed, snapshotv1alpha1.PodGroupConditionReady} {
		if !meta.IsStatusConditionTrue(persistedConditions, condType) && meta.IsStatusConditionTrue(derivedConditions, condType) {
			return true
		}
	}
	final := make(map[string]bool, len(persistedMembers))
	for _, m := range persistedMembers {
		final[m.id] = m.done || m.failed
	}
	for _, m := range derivedMembers {
		if (m.done || m.failed) && !final[m.id] {
			return true
		}
	}
	return false
}

// podGroupAttempt is what the shared rules read of one group in one reconcile.
// newPodGroupAttempt takes the members' views once, before the operator's own
// causes fail the unfinished members, so every rule reads what the members
// reported.
type podGroupAttempt struct {
	kind            *podGroupKind
	deadlineSeconds int64
	admission       podGroupAdmission
	// deleting is true once the group has a deletion timestamp.
	deleting bool
	// pastDeadline is true once spec.deadlineSeconds has passed.
	pastDeadline bool
	outcomes     podGroupMemberOutcomes
}

// podGroupMemberOutcomes is what the members reported.
type podGroupMemberOutcomes struct {
	// committed is true when some member has startedAt.
	committed bool
	// workExists is true when some member's work exists.
	workExists bool
	// allFinal is true when every member succeeded or failed, and allDone when
	// every member succeeded.
	allFinal, allDone bool
	// reported is true when every member is final and none failed with the
	// kind's hiddenStartReason: that member's record is gone, so a start it
	// recorded may never have been seen.
	reported bool
	// notDone counts the members that have not succeeded, of members.
	notDone, members int
	// named is the Failed member that a group failure names (see
	// nameFailedMember), or nil when no member failed.
	named *podGroupMemberView
	// ownFailure is the same among the members whose own work reported the
	// failure, which leaves out the hiddenStartReason.
	ownFailure *podGroupMemberView
}

// newPodGroupAttempt takes the outcomes of members, which are in rank order
// and taken before the operator's own causes fail the unfinished ones. The
// named members point into members.
func newPodGroupAttempt(
	kind *podGroupKind,
	deadlineSeconds int64,
	admission podGroupAdmission,
	deleting, pastDeadline bool,
	members []podGroupMemberView,
) *podGroupAttempt {
	o := podGroupMemberOutcomes{allFinal: true, allDone: true, reported: true, members: len(members)}
	for i := range members {
		m := &members[i]
		o.committed = o.committed || m.started
		o.workExists = o.workExists || m.hasWork
		if !m.done {
			o.allDone = false
			o.notDone++
		}
		if !m.done && !m.failed {
			o.allFinal, o.reported = false, false
		}
		if !m.failed {
			continue
		}
		o.named = nameFailedMember(o.named, m)
		if m.reason == kind.hiddenStartReason {
			o.reported = false
		} else {
			o.ownFailure = nameFailedMember(o.ownFailure, m)
		}
	}
	return &podGroupAttempt{
		kind:            kind,
		deadlineSeconds: deadlineSeconds,
		admission:       admission,
		deleting:        deleting,
		pastDeadline:    pastDeadline,
		outcomes:        o,
	}
}

// memberCause is the cause that fails every unfinished member: the three ways
// the operator itself ends an attempt. It does not depend on the group's
// Failed reason, so a group that failed because of one member still fails the
// others at its deadline. Its message is the member's fixed one, since every
// member gets it; failure explains the group's.
func (a *podGroupAttempt) memberCause() *podGroupCause {
	switch {
	case a.admission.state == podGroupAdmissionRefused:
		return &podGroupCause{a.admission.reason, a.kind.refusedMemberMessage}
	case a.pastDeadline:
		return &podGroupCause{snapshotv1alpha1.PodGroupReasonDeadlineExceeded, a.kind.deadlineMemberMessage}
	case a.deleting && !a.outcomes.committed:
		return &podGroupCause{snapshotv1alpha1.PodGroupReasonCancelled, a.kind.cancelledMemberMessage}
	}
	return nil
}

// updateConditions sets the conditions that both kinds share. Failed is True
// with the first cause, never rewritten afterwards: it is set by the rows of
// failure, unless the group has already failed or every member succeeded.
// Then Admitted:
//
//	True/Committed  once any member has startedAt, since agents record their
//	                start only after the admitted barrier; permanent, because
//	                startedAt is write-once
//	False           with the failure's reason, if the group failed before that
//	False           with the wait's reason, while admission waits
//	False/Admitting once member work exists, or admission passed
func (a *podGroupAttempt) updateConditions(conditions *[]metav1.Condition, now time.Time) {
	var failure *podGroupCause
	if recorded := meta.FindStatusCondition(*conditions, snapshotv1alpha1.PodGroupConditionFailed); recorded != nil &&
		recorded.Status == metav1.ConditionTrue {
		failure = &podGroupCause{recorded.Reason, recorded.Message}
	} else if !a.outcomes.allDone {
		if failure = a.failure(*conditions); failure != nil {
			setPodGroupCondition(conditions, snapshotv1alpha1.PodGroupConditionFailed, metav1.ConditionTrue, *failure, now)
		}
	}

	var status metav1.ConditionStatus
	var cause podGroupCause
	switch {
	case a.outcomes.committed:
		status, cause = metav1.ConditionTrue, podGroupCause{snapshotv1alpha1.PodGroupReasonCommitted, a.kind.committedMessage}
	case failure != nil:
		status, cause = metav1.ConditionFalse, *failure
	case a.admission.state == podGroupAdmissionWaiting:
		status, cause = metav1.ConditionFalse, podGroupCause{a.admission.reason, a.admission.message}
	case a.outcomes.workExists || a.admission.state == podGroupAdmissionPassed:
		status, cause = metav1.ConditionFalse, podGroupCause{snapshotv1alpha1.PodGroupReasonAdmitting, a.kind.admittingMessage}
	default:
		return
	}
	setPodGroupCondition(conditions, snapshotv1alpha1.PodGroupConditionAdmitted, status, cause, now)
}

// failure is the group's failure cause, or nil while it has none. conditions
// are the group's conditions before this reconcile set Admitted. The first
// matching row wins:
//
//	1  admission refused                      the refusal's reason
//	2  committed, and some member Failed,     the named member's reason if
//	   hiddenStartReason included             CoordinatorLost, DeadlineExceeded,
//	                                          Cancelled or PodIPChanged, otherwise
//	                                          MemberFailed
//	3  not committed, and reported            the named member's reason if
//	                                          GateBudgetTooShort or PodIPChanged,
//	                                          otherwise Refused
//	4  the deadline passed                    DeadlineExceeded
//	5  deleted, and not committed             Cancelled
//	6  every member final, some member Failed MemberFailed
//
// GateBudgetTooShort and PodIPChanged are a restore member's group checks,
// which a checkpoint member never reports. Rows 2, 3 and 6 name the named
// member, with its reason and message. Row 4 adds the member that had already
// failed on its own, if any. Refused is certain: it needs every member to have
// reported, so none can still start, and none whose record of its start may
// be gone. A finished attempt never waits for its deadline: once every member
// is final, row 2, 3, 5 or 6 applies.
func (a *podGroupAttempt) failure(conditions []metav1.Condition) *podGroupCause {
	outcomes := a.outcomes
	named := outcomes.named
	switch {
	case a.admission.state == podGroupAdmissionRefused:
		return &podGroupCause{a.admission.reason, a.admission.message}
	case outcomes.committed && named != nil:
		reason := snapshotv1alpha1.PodGroupReasonMemberFailed
		switch named.reason {
		case snapshotv1alpha1.PodGroupReasonCoordinatorLost,
			snapshotv1alpha1.PodGroupReasonDeadlineExceeded,
			snapshotv1alpha1.PodGroupReasonCancelled,
			snapshotv1alpha1.PodGroupReasonPodIPChanged:
			reason = named.reason
		}
		return &podGroupCause{reason, podGroupMemberFailureMessage(named)}
	case !outcomes.committed && outcomes.reported && named != nil:
		reason := snapshotv1alpha1.PodGroupReasonRefused
		switch named.reason {
		case snapshotv1alpha1.PodGroupReasonGateBudgetTooShort,
			snapshotv1alpha1.PodGroupReasonPodIPChanged:
			reason = named.reason
		}
		return &podGroupCause{reason, podGroupMemberFailureMessage(named)}
	case a.pastDeadline:
		message := a.deadlineMessage(conditions)
		if outcomes.ownFailure != nil {
			message += "; " + podGroupMemberFailureMessage(outcomes.ownFailure)
		}
		return &podGroupCause{snapshotv1alpha1.PodGroupReasonDeadlineExceeded, message}
	case a.deleting && !outcomes.committed:
		return &podGroupCause{snapshotv1alpha1.PodGroupReasonCancelled, a.kind.cancelledMessage}
	case outcomes.allFinal && named != nil:
		return &podGroupCause{snapshotv1alpha1.PodGroupReasonMemberFailed, podGroupMemberFailureMessage(named)}
	}
	return nil
}

// nameFailedMember returns which of two Failed members, named before m in rank
// order, a group failure names. A member with a cause of its own wins over one
// with a group reason, which only says how the group's session ended for it;
// otherwise the first in rank order wins.
func nameFailedMember(named, m *podGroupMemberView) *podGroupMemberView {
	if named == nil || (isPodGroupSessionReason(named.reason) && !isPodGroupSessionReason(m.reason)) {
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

// podGroupMemberFailureMessage names a Failed member, with its reason and
// message.
func podGroupMemberFailureMessage(m *podGroupMemberView) string {
	return fmt.Sprintf("member %q (rank %d) failed with %s: %s", m.id, m.rank, m.reason, m.message)
}

// deadlineMessage says how far the group got by its deadline. A group that
// never got past admission says what admission was waiting for, which is the
// usual reason a deadline expires that early.
func (a *podGroupAttempt) deadlineMessage(conditions []metav1.Condition) string {
	if admitted := meta.FindStatusCondition(conditions, snapshotv1alpha1.PodGroupConditionAdmitted); admitted != nil &&
		admitted.Status == metav1.ConditionFalse && isPodGroupAdmissionWaitReason(admitted.Reason) {
		return fmt.Sprintf("the deadline of %d seconds passed while admission was waiting: %s",
			a.deadlineSeconds, admitted.Message)
	}
	return fmt.Sprintf("the deadline of %d seconds passed with %d of %d members not %s",
		a.deadlineSeconds, a.outcomes.notDone, a.outcomes.members, a.kind.done)
}

// isPodGroupAdmissionWaitReason reports whether reason is one that admission
// waits with.
func isPodGroupAdmissionWaitReason(reason string) bool {
	switch reason {
	case snapshotv1alpha1.PodGroupReasonPreviousGroupCleanup,
		snapshotv1alpha1.PodGroupReasonSourcePodUnscheduled,
		snapshotv1alpha1.PodGroupReasonTargetPodPending,
		snapshotv1alpha1.PodGroupReasonTargetPodUnscheduled,
		snapshotv1alpha1.PodGroupReasonAgentNotReady:
		return true
	}
	return false
}

// podGroupEvent is an event for one transition of a group's conditions.
type podGroupEvent struct {
	eventType string
	reason    string
	message   string
}

// podGroupTransitions lists the events for the transitions of a group's
// conditions from previous to next: Admitted, Released, Ready and Failed
// turning True, and Ready turning False after it was True, which only a
// PodGroupSnapshot's can. Each event carries the condition's reason and
// message, so a reconcile that changes nothing emits nothing.
func podGroupTransitions(previous, next []metav1.Condition) []podGroupEvent {
	var events []podGroupEvent
	for _, t := range []struct {
		condType  string
		eventType string
	}{
		{snapshotv1alpha1.PodGroupConditionAdmitted, corev1.EventTypeNormal},
		{snapshotv1alpha1.PodGroupConditionReleased, corev1.EventTypeNormal},
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

// setPodGroupCondition sets a condition on a group's conditions, with its
// message cut to the API server's limit.
func setPodGroupCondition(
	conditions *[]metav1.Condition,
	condType string,
	conditionStatus metav1.ConditionStatus,
	cause podGroupCause,
	now time.Time,
) {
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               condType,
		Status:             conditionStatus,
		Reason:             cause.reason,
		Message:            capMessage(cause.message, maxConditionMessageLength),
		LastTransitionTime: metav1.NewTime(now),
	})
}

// capMessage cuts a message to limit bytes, dropping any character the cut
// splits. The API server counts characters, so the cut message always fits. A
// member's own message can be longer than a member's limit, and a group's
// message adds to a member's.
func capMessage(message string, limit int) string {
	if len(message) <= limit {
		return message
	}
	return strings.ToValidUTF8(message[:limit], "")
}

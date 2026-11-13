// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// podGroupRestoreMemberObservation is what one reconcile saw of one pinned
// target.
type podGroupRestoreMemberObservation struct {
	// report is what this restore's agent reported on the member's pinned
	// Pod; empty when the member is not pinned, the cache holds no such Pod,
	// or the Pod carries no report of this attempt.
	report podGroupRestoreReport
	// podDeleted is true when the API server confirmed that the member's
	// pinned Pod is gone, or replaced by another Pod.
	podDeleted bool
}

// podGroupRestoreReport is one attempt's report on its restore Pod: the
// conditions its agent writes. Each is nil when the report lacks it.
type podGroupRestoreReport struct {
	started, restored, groupFailed *corev1.PodCondition
}

// podGroupRestoreReportOf returns the report of the attempt with uid on pod:
// its RestoreStarted, Restored and GroupRestoreFailed conditions, only while
// the Pod's binding, read by podcontract.RestoreGroupClaimOf, holds uid. A
// claimed Pod serves only the attempt its binding names, so its conditions
// need no identity of their own; a Pod bound to another attempt, or whose
// claim is invalid, yields an empty report.
func podGroupRestoreReportOf(pod *corev1.Pod, uid types.UID) podGroupRestoreReport {
	claim, bound, err := podcontract.RestoreGroupClaimOf(pod.Annotations)
	if uid == "" || !bound || err != nil || claim.UID != uid {
		return podGroupRestoreReport{}
	}
	return podGroupRestoreReport{
		started:     findPodCondition(pod, podcontract.RestoreStartedCondition),
		restored:    findPodCondition(pod, podcontract.RestoredCondition),
		groupFailed: findPodCondition(pod, podcontract.GroupRestoreFailedCondition),
	}
}

// podGroupRestoreObservation is everything derivePodGroupRestoreStatus reads
// besides the spec and the previous status.
type podGroupRestoreObservation struct {
	// members holds one entry per spec target, in spec order.
	members []podGroupRestoreMemberObservation
	// ranks maps the ID of each member of the PodGroupSnapshot to its rank.
	// Nil when the snapshot is missing or is another object.
	ranks     map[string]int
	admission podGroupAdmission
	// pins holds, once admission passed, what to pin for each spec target, in
	// spec order: pod, nodeName, containers and targetPodIP.
	pins []snapshotv1alpha1.PodGroupRestoreMemberStatus
	// claims holds the target Pods that admission prepared to claim, which
	// the reconcile sends after it records the status. The status does not
	// read them.
	claims []*corev1.Pod
	// deleting is true once the restore has a deletion timestamp.
	deleting bool
	// deadline is the restore's creation time plus spec.deadlineSeconds.
	deadline time.Time
}

// member returns the observation of the spec target at index i, or an empty
// one.
func (o podGroupRestoreObservation) member(i int) podGroupRestoreMemberObservation {
	if i < len(o.members) {
		return o.members[i]
	}
	return podGroupRestoreMemberObservation{}
}

// The messages of a member failed by a cause of the whole restore, and the
// restore's message when it is deleted before any member started.
const (
	podGroupRestoreRefusedMemberMessage   = "the PodGroupRestore was refused; see its Failed condition"
	podGroupRestoreDeadlineMemberMessage  = "the PodGroupRestore's deadline passed; see its Failed condition"
	podGroupRestoreCancelledMemberMessage = "the PodGroupRestore was deleted before any member started; see its Failed condition"
	podGroupRestoreCancelledMessage       = "the PodGroupRestore was deleted before any member started"
)

// podGroupRestoreKind is what the shared rules say for a PodGroupRestore.
var podGroupRestoreKind = podGroupKind{
	hiddenStartReason:      snapshotv1alpha1.PodGroupReasonTargetPodDeleted,
	done:                   "released",
	refusedMemberMessage:   podGroupRestoreRefusedMemberMessage,
	deadlineMemberMessage:  podGroupRestoreDeadlineMemberMessage,
	cancelledMemberMessage: podGroupRestoreCancelledMemberMessage,
	cancelledMessage:       podGroupRestoreCancelledMessage,
	committedMessage:       "every member passed admission, and the restore has started",
	admittingMessage:       "every member is pinned to its restore Pod; waiting for every member to pass admission",
}

// derivePodGroupRestoreStatus computes a PodGroupRestore's status from its
// spec, one reconcile's observations and the previous status, at time now. It
// is pure: it makes no client calls, so a restarted operator derives the same
// status from the same objects.
//
// The status lists the members in spec order. When admission passes, it pins
// every member at once, in the one status patch this status becomes. A member
// reads only its report: the conditions this attempt's agent wrote on the
// pinned Pod, while the Pod's binding holds this restore's UID (see
// podGroupRestoreReportOf). Each member's phase then comes from the first
// matching row:
//
//	Failed     its report's nvidia.com/Restored is False with RestoreFailed or
//	           RestoreIncompatible: the reason and message of the report's
//	           nvidia.com/GroupRestoreFailed when it is True, otherwise
//	           Restored's, the message cut to maxMemberMessageLength bytes
//	Failed     its pinned Pod is gone or replaced, as the API server
//	           confirmed: TargetPodDeleted
//	Failed     the restore was refused, passed its deadline, or was deleted
//	           before any member started, and the member is not Released:
//	           that cause's reason, with a fixed message pointing to the
//	           restore's Failed condition
//	Released   its report's nvidia.com/Restored is True
//	Restoring  it has startedAt
//	Admitted   it is pinned
//	Pending    otherwise
//
// Any other Restored is not final: absent, RestoreInProgress, or an agent's
// internal wait such as GroupPending. GroupRestoreFailed alone fails nothing:
// it only says why Restored failed. Released and Failed are final, and the
// pins and startedAt are write-once. startedAt is the lastTransitionTime of the
// report's nvidia.com/RestoreStarted condition, copied whenever it is seen, even
// on a member that is already Failed, since a member can fail before the
// operator ever sees it Restoring.
//
// The restore's Failed and Admitted conditions follow the rules both kinds
// share (see podGroupAttempt.updateConditions), and these are the restore's
// own:
//
//	Released  True once any member is Released, since agents release only
//	          after the release barrier.
//	Ready     True once every member is Released. Never withdrawn.
func derivePodGroupRestoreStatus(
	spec *snapshotv1alpha1.PodGroupRestoreSpec,
	observed podGroupRestoreObservation,
	previous snapshotv1alpha1.PodGroupRestoreStatus,
	now time.Time,
) snapshotv1alpha1.PodGroupRestoreStatus {
	next := previous.DeepCopy()
	previousMembers := make(map[string]snapshotv1alpha1.PodGroupRestoreMemberStatus, len(next.Members))
	for _, m := range next.Members {
		previousMembers[m.ID] = m
	}

	next.Members = make([]snapshotv1alpha1.PodGroupRestoreMemberStatus, len(spec.Targets))
	for i, target := range spec.Targets {
		status := previousMembers[target.ID]
		status.ID = target.ID
		if status.Phase == "" {
			status.Phase = snapshotv1alpha1.RestorePhasePending
		}
		if observed.admission.state == podGroupAdmissionPassed && status.Pod == nil {
			pin := observed.pins[i]
			status.Pod, status.NodeName, status.Containers, status.TargetPodIP = pin.Pod, pin.NodeName, pin.Containers, pin.TargetPodIP
		}
		derivePodGroupRestoreMemberStatus(&status, observed.member(i))
		next.Members[i] = status
	}

	attempt := newPodGroupAttempt(&podGroupRestoreKind, spec.DeadlineSeconds, observed.admission,
		observed.deleting, !now.Before(observed.deadline), podGroupRestoreMemberViews(next.Members, observed.ranks))
	if cause := attempt.memberCause(); cause != nil {
		for i := range next.Members {
			if m := &next.Members[i]; !isRestorePhaseFinal(m.Phase) {
				m.Phase, m.Reason, m.Message = snapshotv1alpha1.RestorePhaseFailed, cause.reason, cause.message
			}
		}
	}

	attempt.updateConditions(&next.Conditions, now)

	if slices.ContainsFunc(next.Members, func(m snapshotv1alpha1.PodGroupRestoreMemberStatus) bool {
		return m.Phase == snapshotv1alpha1.RestorePhaseReleased
	}) {
		setPodGroupCondition(&next.Conditions, snapshotv1alpha1.PodGroupConditionReleased, metav1.ConditionTrue,
			podGroupCause{string(snapshotv1alpha1.RestorePhaseReleased),
				"the release barrier completed; released members keep running whatever happens to the others"}, now)
	}
	if attempt.outcomes.allDone {
		setPodGroupCondition(&next.Conditions, snapshotv1alpha1.PodGroupConditionReady, metav1.ConditionTrue,
			podGroupCause{string(snapshotv1alpha1.RestorePhaseReleased),
				fmt.Sprintf("all %d members are released", len(next.Members))}, now)
	}
	return *next
}

// derivePodGroupRestoreMemberStatus fills in a member's startedAt from its
// report and moves its phase by the member table.
func derivePodGroupRestoreMemberStatus(m *snapshotv1alpha1.PodGroupRestoreMemberStatus, observed podGroupRestoreMemberObservation) {
	report := observed.report
	if started := report.started; started != nil && started.Status == corev1.ConditionTrue && m.StartedAt == nil {
		m.StartedAt = started.LastTransitionTime.DeepCopy()
	}

	if isRestorePhaseFinal(m.Phase) {
		return
	}
	m.Reason, m.Message = "", ""
	restored := report.restored
	switch {
	case restored != nil && restored.Status == corev1.ConditionFalse &&
		(restored.Reason == podcontract.RestoreReasonFailed || restored.Reason == podcontract.RestoreReasonIncompatible):
		cause := restored
		if group := report.groupFailed; group != nil && group.Status == corev1.ConditionTrue {
			cause = group
		}
		m.Phase, m.Reason = snapshotv1alpha1.RestorePhaseFailed, cause.Reason
		m.Message = capMessage(cause.Message, maxMemberMessageLength)
	case observed.podDeleted && m.Pod != nil:
		m.Phase, m.Reason = snapshotv1alpha1.RestorePhaseFailed, snapshotv1alpha1.PodGroupReasonTargetPodDeleted
		m.Message = fmt.Sprintf("Pod %q was deleted or replaced before the member was released", m.Pod.Name)
	case restored != nil && restored.Status == corev1.ConditionTrue:
		m.Phase = snapshotv1alpha1.RestorePhaseReleased
	case m.StartedAt != nil:
		m.Phase = snapshotv1alpha1.RestorePhaseRestoring
	case m.Pod != nil:
		m.Phase = snapshotv1alpha1.RestorePhaseAdmitted
	default:
		m.Phase = snapshotv1alpha1.RestorePhasePending
	}
}

// findPodCondition returns the Pod's condition of type condType, or nil.
func findPodCondition(pod *corev1.Pod, condType string) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == corev1.PodConditionType(condType) {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}

// podGroupRestoreMemberViews returns the shared rules' view of each member, in
// rank order: each member takes the rank of the PodGroupSnapshot member with
// the same ID. When the snapshot is not known, which refuses the restore, or a
// target matches no member, the target's rank is unknown: it takes a rank
// after every known one, by its position in the spec.
func podGroupRestoreMemberViews(members []snapshotv1alpha1.PodGroupRestoreMemberStatus, ranks map[string]int) []podGroupMemberView {
	views := make([]podGroupMemberView, len(members))
	for i, m := range members {
		rank, known := ranks[m.ID]
		if !known {
			rank = len(ranks) + i
		}
		views[i] = podGroupMemberView{
			id:      m.ID,
			rank:    int32(rank),
			started: m.StartedAt != nil,
			hasWork: m.Pod != nil,
			done:    m.Phase == snapshotv1alpha1.RestorePhaseReleased,
			failed:  m.Phase == snapshotv1alpha1.RestorePhaseFailed,
			reason:  m.Reason,
			message: m.Message,
		}
	}
	slices.SortFunc(views, func(a, b podGroupMemberView) int { return cmp.Compare(a.rank, b.rank) })
	return views
}

// isRestorePhaseFinal reports whether a member's phase can no longer change.
func isRestorePhaseFinal(phase snapshotv1alpha1.RestorePhase) bool {
	return phase == snapshotv1alpha1.RestorePhaseReleased || phase == snapshotv1alpha1.RestorePhaseFailed
}

// podGroupRestoreFinished reports whether a PodGroupRestore's attempt has
// finished: it is Ready or Failed, and no member is still Restoring. Agents
// read an unfinished restore for its pins until their member ends, so it holds
// its own in-use finalizer, and its PodGroupSnapshot's, and its target Pods stay
// in use. A restore deleted before any member started has finished with
// Cancelled; its deadline bounds every other wait, since it fails every member
// still Restoring.
func podGroupRestoreFinished(status *snapshotv1alpha1.PodGroupRestoreStatus) bool {
	ended := meta.IsStatusConditionTrue(status.Conditions, snapshotv1alpha1.PodGroupConditionReady) ||
		meta.IsStatusConditionTrue(status.Conditions, snapshotv1alpha1.PodGroupConditionFailed)
	if !ended {
		return false
	}
	for _, m := range status.Members {
		if m.Phase == snapshotv1alpha1.RestorePhaseRestoring {
			return false
		}
	}
	return true
}

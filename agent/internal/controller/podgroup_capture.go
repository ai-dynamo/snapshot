// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	"github.com/ai-dynamo/snapshot/agent/internal/group"
	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// captureMember is a PodSnapshotContent's part in the capture of a
// PodGroupSnapshot.
type captureMember struct {
	// group names the PodGroupSnapshot, and attempt is its UID, which names
	// the session.
	group   client.ObjectKey
	attempt ktypes.UID
	// id is the member's ID.
	id string
	// deadline is the PodGroupSnapshot's creation plus spec.deadlineSeconds.
	deadline time.Time
}

// invalidMemberError reports a PodSnapshotContent with invalid group membership.
type invalidMemberError struct{ message string }

func (e *invalidMemberError) Error() string { return e.message }

func invalidMember(format string, args ...any) error {
	return &invalidMemberError{message: fmt.Sprintf(format, args...)}
}

// captureMemberRetryInterval paces registration checks while the group
// controller records the child's identity.
const captureMemberRetryInterval = time.Second

// captureMemberOf resolves content's immutable membership. registered is false
// only while a valid member waits for its status pin; member then carries the
// deadline that bounds the wait. A nil member with registered true is standalone.
func (w *NodeController) captureMemberOf(ctx context.Context, content *snapshotv1alpha1.PodSnapshotContent) (*captureMember, bool, error) {
	ref := content.Spec.PodGroup
	if ref == nil {
		return nil, true, nil
	}
	if w.podGroups == nil {
		return nil, false, invalidMember("content %s names a pod group, but pod groups are off", content.Name)
	}
	if ref.Name == "" || ref.UID == "" || ref.MemberID == "" {
		return nil, false, invalidMember("content %s has incomplete pod group membership", content.Name)
	}
	snapshot := content.Spec.PodSnapshotRef
	if snapshot.UID == "" {
		return nil, false, invalidMember("group member content %s has no PodSnapshot UID", content.Name)
	}
	key := client.ObjectKey{Namespace: snapshot.Namespace, Name: ref.Name}
	podGroup := &snapshotv1alpha1.PodGroupSnapshot{}
	if err := w.client.Get(ctx, key, podGroup); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, invalidMember("PodGroupSnapshot %s of content %s is gone", key, content.Name)
		}
		return nil, false, fmt.Errorf("read PodGroupSnapshot %s: %w", key, err)
	}
	if podGroup.UID != ref.UID {
		return nil, false, invalidMember("PodGroupSnapshot %s has UID %s, not %s", key, podGroup.UID, ref.UID)
	}
	rank := slices.IndexFunc(podGroup.Spec.Members, func(m snapshotv1alpha1.PodGroupSnapshotMember) bool {
		return m.ID == ref.MemberID
	})
	if rank < 0 {
		return nil, false, invalidMember("PodGroupSnapshot %s has no member %q", key, ref.MemberID)
	}
	source, captured := podGroup.Spec.Members[rank].Source.PodRef, content.Spec.Source.PodRef
	if source.Name != captured.Name || source.UID != captured.UID || !slices.Equal(source.Containers, captured.Containers) {
		return nil, false, invalidMember("member %q of PodGroupSnapshot %s has source Pod %s (uid %s), not %s (uid %s), or different containers",
			ref.MemberID, key, source.Name, source.UID, captured.Name, captured.UID)
	}
	member := &captureMember{
		group: key, attempt: ref.UID, id: ref.MemberID,
		deadline: podGroup.CreationTimestamp.Add(time.Duration(podGroup.Spec.DeadlineSeconds) * time.Second),
	}
	i := slices.IndexFunc(podGroup.Status.Members, func(m snapshotv1alpha1.PodGroupSnapshotMemberStatus) bool {
		return m.ID == ref.MemberID
	})
	if i < 0 || podGroup.Status.Members[i].PodSnapshot == nil {
		return member, false, nil
	}
	pin := podGroup.Status.Members[i].PodSnapshot
	if pin.Name != snapshot.Name || pin.UID != snapshot.UID {
		return nil, false, invalidMember("member %q of PodGroupSnapshot %s pins PodSnapshot %s (uid %s), not %s (uid %s)",
			ref.MemberID, key, pin.Name, pin.UID, snapshot.Name, snapshot.UID)
	}
	return member, true, nil
}

// forgetCaptureOutcome drops a retained result once its content is final or gone.
func (w *NodeController) forgetCaptureOutcome(name string) {
	if w.podGroups == nil {
		return
	}
	w.podGroups.captureOutcomes.Delete(name)
}

// captureSessionNode reads the node of the rank-0 member of m's
// PodGroupSnapshot, the node whose coordinator holds the session, as the
// coordinator reads it.
func (w *NodeController) captureSessionNode(m *captureMember) group.SessionNode {
	return func(ctx context.Context) (string, error) {
		podGroup := &snapshotv1alpha1.PodGroupSnapshot{}
		if err := w.client.Get(ctx, m.group, podGroup); err != nil {
			if apierrors.IsNotFound(err) {
				return "", fmt.Errorf("PodGroupSnapshot %s: %w", m.group, group.ErrNoAttempt)
			}
			return "", err
		}
		if podGroup.UID != m.attempt {
			return "", fmt.Errorf("PodGroupSnapshot %s was replaced: %w", m.group, group.ErrNoAttempt)
		}
		if len(podGroup.Spec.Members) == 0 {
			return "", nil
		}
		rankZero := podGroup.Spec.Members[0].ID
		for _, status := range podGroup.Status.Members {
			if status.ID == rankZero {
				return status.NodeName, nil
			}
		}
		return "", nil
	}
}

// captureOutcome is the end of a member's capture that this agent ran.
type captureOutcome struct {
	uid ktypes.UID
	// ready is set for a capture that committed its artifact.
	ready         bool
	artifactPath  string
	containerName string
	artifact      *pagebroker.PublishedArtifact
	// reason and cause are a failed capture's.
	reason string
	cause  error
}

// recordCaptureOutcome writes outcome on content, and keeps it until the
// content is final, so a write that does not land is sent again rather than
// the member checkpointed again.
func (w *NodeController) recordCaptureOutcome(ctx context.Context, content *snapshotv1alpha1.PodSnapshotContent, outcome *captureOutcome) error {
	w.podGroups.captureOutcomes.Store(content.Name, outcome)
	if outcome.ready {
		return w.markCheckpointReady(ctx, content, outcome.artifactPath, outcome.containerName, outcome.artifact)
	}
	return w.markCheckpointFailed(ctx, content, outcome.reason, outcome.cause)
}

// pendingCaptureOutcome returns the outcome this agent holds for content, or
// nil.
func (w *NodeController) pendingCaptureOutcome(content *snapshotv1alpha1.PodSnapshotContent) *captureOutcome {
	if w.podGroups == nil {
		return nil
	}
	held, ok := w.podGroups.captureOutcomes.Load(content.Name)
	if !ok || held.(*captureOutcome).uid != content.UID {
		return nil
	}
	return held.(*captureOutcome)
}

// failInterruptedCapture fails a member that had started when this agent
// restarted, and that no outcome of this agent covers. Its source may be
// suspended mid-capture, so it is terminated, as a started member that fails
// always is; the member is never checkpointed again.
func (w *NodeController) failInterruptedCapture(
	ctx context.Context, content *snapshotv1alpha1.PodSnapshotContent, pod *corev1.Pod, containerName string,
) error {
	logger := logr.FromContextOrDiscard(ctx)
	if containerID := containerIDForName(pod, containerName); containerID != "" {
		if pid, _, err := w.runtime.ResolveContainer(ctx, containerID); err == nil {
			w.recoverSourceAfterFailedDump(ctx, logger, pod, pid, executor.SuspendedSource{}, false)
		} else {
			logger.Info("Cannot resolve the interrupted member's source; it may have ended", "error", err.Error())
		}
	}
	emitPodEvent(ctx, w.clientset, logger, pod, snapshotEventComponent, corev1.EventTypeWarning, reasonCheckpointInterrupted,
		"Checkpoint failed: the agent restarted after the pod group member started")
	return w.recordCaptureOutcome(ctx, content, &captureOutcome{
		uid:    content.UID,
		reason: reasonCheckpointInterrupted,
		cause:  errors.New("the agent restarted after the member started; a member that started is never checkpointed again"),
	})
}

// contentStarted reports whether a member's PodSnapshotContent records that
// the member started.
func contentStarted(content *snapshotv1alpha1.PodSnapshotContent) bool {
	return meta.IsStatusConditionTrue(content.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionStarted)
}

// runGroupCheckpoint checkpoints a pod group member: it joins the attempt and
// runs the dump as the member's part in the group's capture. The attempt
// commits at the admitted barrier, and the member records that it started,
// the Started condition on its content, before its first irreversible step.
// The attempt's deadline alone bounds the member, not checkpoint.timeoutSeconds,
// so a member that runs out of time fails as the group does.
//
// There is no second attempt: the session is single-use, and a member that
// fails fails the attempt for every member. Repeats of the dump against the
// suspended workload still spend the capture's retry budget. On success the
// member leaves the attempt off the queue worker.
//
// A failed member reports its own reason, or the attempt's when it failed
// because the attempt ended. A member that never started is left as Snapshot
// found it, without the capture-eligible label; one that started is
// terminated. A panic after the member joined fails it in the same way.
func (w *NodeController) runGroupCheckpoint(
	ctx context.Context,
	content *snapshotv1alpha1.PodSnapshotContent,
	pod *corev1.Pod,
	member *captureMember,
	containerName, containerID string,
	containerPID int,
	cuInterposeRequested bool,
	contentUID, artifactPath string,
) (retErr error) {
	logger := logr.FromContextOrDiscard(ctx).WithValues("attempt", member.attempt, "member", member.id)
	ctx = logr.NewContext(ctx, logger)
	captureCtx, cancel := context.WithDeadline(ctx, member.deadline)
	defer cancel()

	g, err := w.joinAttempt(captureCtx, member.attempt, member.id, w.captureSessionNode(member))
	if err != nil {
		reason, _ := joinFailureReason(err, member.deadline, w.nowFn())
		w.leaveSourceAsFound(ctx, pod)
		return w.failGroupCheckpoint(ctx, logger, content, pod, reason, fmt.Errorf("join the pod group's attempt: %w", err))
	}
	logger.Info("Joined the pod group's attempt", "rank", g.Rank())

	var recorded atomic.Bool
	// As restoreGroupMember does: without this, a member that panicked keeps
	// its session open and a source that started suspended.
	defer recoverPanic(logger, "group checkpoint", func(cause error) {
		retErr = w.failGroupCheckpointAfterPanic(ctx, logger, content, pod, g, recorded.Load(), containerPID, cause)
	})
	storeID := ""
	if content.Spec.Storage != nil {
		storeID = content.Spec.Storage.StoreID
	}
	budget := w.config.Checkpoint.ResolveRetryLimit()
	artifact, err := w.checkpointFn(captureCtx, CheckpointParams{
		Pod:                  pod,
		ContainerName:        containerName,
		ContainerID:          containerID,
		ContainerPID:         containerPID,
		ContentUID:           contentUID,
		HostPath:             artifactPath,
		StartedAt:            time.Now(),
		CuInterposeRequested: cuInterposeRequested,
		StoreID:              storeID,
		DumpRetriesRemaining: budget,
		DumpWorthRepeating:   w.dumpWorthRepeating(ctx, containerPID),
		OnDumpRepeat: func(repeat int, dumpErr error) {
			refined, _ := w.refineFullStaging(ctx, classifyCaptureFailure(dumpErr), dumpErr, containerPID)
			w.reportAttemptFailed(ctx, logger, pod, refined.reason, repeat, budget, dumpErr)
		},
		Group: g,
		Committed: func(ctx context.Context) error {
			if err := w.recordCaptureStarted(ctx, content); err != nil {
				return err
			}
			recorded.Store(true)
			return nil
		},
	})
	if err == nil {
		w.leaveAttempt(ctx, logger, g)
		return w.recordCaptureOutcome(ctx, content, &captureOutcome{
			uid: content.UID, ready: true, artifactPath: artifactPath, containerName: containerName, artifact: artifact,
		})
	}
	failure, err := w.refineFullStaging(ctx, classifyCaptureFailure(err), err, containerPID)
	attempt := settleAttempt(failure, err)
	reason := attempt.failure.reason
	if ended := attemptEndReason(err, member.deadline, w.nowFn()); ended != "" {
		reason = ended
	}
	return w.endFailedMember(ctx, logger, content, pod, g, containerPID,
		recorded.Load() || attempt.suspendedSource, attempt.suspended, reason, err)
}

// endFailedMember ends a member whose capture failed after it joined its
// attempt. It fails the attempt, which closes the session. It terminates the
// source of a member that started, or that the failure left suspended, and
// leaves any other source as found. Then it records the failure, with reason,
// on the content.
func (w *NodeController) endFailedMember(
	ctx context.Context, logger logr.Logger, content *snapshotv1alpha1.PodSnapshotContent, pod *corev1.Pod,
	g *group.Member, containerPID int, terminate bool, suspended executor.SuspendedSource, reason string, cause error,
) error {
	failAttempt(ctx, logger, g)
	if terminate {
		w.recoverSourceAfterFailedDump(ctx, logger, pod, containerPID, suspended, false)
	} else {
		w.leaveSourceAsFound(ctx, pod)
	}
	return w.failGroupCheckpoint(ctx, logger, content, pod, reason, cause)
}

// failBeforeJoin fails content before its capture starts. A pod group member
// that fails here never joins its attempt: its source is left as Snapshot
// found it. Once the group's status records the failure, the coordinator ends
// the attempt with its group reason, or Refused for a member-specific reason.
func (w *NodeController) failBeforeJoin(
	ctx context.Context, content *snapshotv1alpha1.PodSnapshotContent, pod *corev1.Pod, member *captureMember, reason string, cause error,
) error {
	w.leaveMemberSourceAsFound(ctx, member, pod)
	return w.setSnapshotContentFailed(ctx, content, reason, cause)
}

// leaveMemberSourceAsFound leaves the source of a pod group member that never
// started as Snapshot found it. It does nothing for a content captured on its
// own.
func (w *NodeController) leaveMemberSourceAsFound(ctx context.Context, member *captureMember, pod *corev1.Pod) {
	if member != nil {
		w.leaveSourceAsFound(ctx, pod)
	}
}

// leaveUnstartedMemberSource leaves a member's source as found after a panic
// or deletion. The label may be from an earlier reconcile, so it reads the
// source again and checks its UID.
func (w *NodeController) leaveUnstartedMemberSource(ctx context.Context, content *snapshotv1alpha1.PodSnapshotContent) {
	if contentStarted(content) {
		return
	}
	member, _, err := w.captureMemberOf(ctx, content)
	if err != nil || member == nil {
		return
	}
	pod := &corev1.Pod{}
	key := client.ObjectKey{Namespace: content.Spec.PodSnapshotRef.Namespace, Name: content.Spec.Source.PodRef.Name}
	if err := w.client.Get(ctx, key, pod); err != nil {
		w.log.Error(err, "Failed to read the source of a pod group member that never started", "pod", key.String())
		return
	}
	if reason, _ := classifySourcePodIdentity(content, pod); reason == "" {
		w.removeCaptureEligibleLabel(ctx, pod)
	}
}

// leaveSourceAsFound removes the capture-eligible label from the source of a
// member that never started. reconcileCapture added it before the readiness
// wait, to the Pod on the API server rather than to pod, so it is removed
// whatever pod's labels say.
func (w *NodeController) leaveSourceAsFound(ctx context.Context, pod *corev1.Pod) {
	labeled := podLabelPatchBase(pod)
	if labeled.Labels == nil {
		labeled.Labels = map[string]string{}
	}
	labeled.Labels[snapshotv1alpha1.CaptureEligibleLabel] = "true"
	w.removeCaptureEligibleLabel(ctx, labeled)
}

// failGroupCheckpoint records a member's failure on its content, and tells the
// source Pod's owner, as failCheckpoint does for a Pod checkpointed on its
// own.
func (w *NodeController) failGroupCheckpoint(
	ctx context.Context, logger logr.Logger, content *snapshotv1alpha1.PodSnapshotContent, pod *corev1.Pod, reason string, cause error,
) error {
	logWithStep(logger, cause).Error(cause, "Pod group member's checkpoint failed", "reason", reason)
	emitPodEvent(ctx, w.clientset, logger, pod, snapshotEventComponent, corev1.EventTypeWarning,
		reason, fmt.Sprintf("Checkpoint failed: %v", cause))
	return w.recordCaptureOutcome(ctx, content, &captureOutcome{uid: content.UID, reason: reason, cause: cause})
}

// failGroupCheckpointAfterPanic fails a member whose capture panicked after it
// joined its attempt, as endFailedMember ends any failed member, and records
// AgentPanic. A capture that had already concluded keeps the outcome it holds,
// which the next reconcile records.
func (w *NodeController) failGroupCheckpointAfterPanic(
	ctx context.Context, logger logr.Logger, content *snapshotv1alpha1.PodSnapshotContent, pod *corev1.Pod,
	g *group.Member, started bool, containerPID int, cause error,
) error {
	if w.pendingCaptureOutcome(content) != nil {
		return cause
	}
	return w.endFailedMember(ctx, logger, content, pod, g, containerPID, started, executor.SuspendedSource{}, reasonAgentPanic, cause)
}

// recordCaptureStarted sets the Started condition on a member's content: the
// record, written once and never removed, that the member crossed its point of
// no return. It writes through content, so the capture's later status writes
// start from the content it wrote.
func (w *NodeController) recordCaptureStarted(ctx context.Context, content *snapshotv1alpha1.PodSnapshotContent) error {
	current := content.DeepCopy()
	for range readyStatusConflictLimit {
		if contentStarted(current) {
			*content = *current
			return nil
		}
		if isContentTerminal(current) {
			return fmt.Errorf("PodSnapshotContent %s ended before the member started", current.Name)
		}
		patch := client.MergeFromWithOptions(current.DeepCopy(), client.MergeFromWithOptimisticLock{})
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type:    snapshotv1alpha1.PodSnapshotConditionStarted,
			Status:  metav1.ConditionTrue,
			Reason:  "CheckpointStarted",
			Message: "Every member of the pod group passed admission; checkpointing this member",
		})
		err := w.client.Status().Patch(ctx, current, patch)
		if err == nil {
			*content = *current
			return nil
		}
		if !apierrors.IsConflict(err) {
			return fmt.Errorf("record that the member started on PodSnapshotContent %s: %w", content.Name, err)
		}
		current = &snapshotv1alpha1.PodSnapshotContent{}
		if err := w.client.Get(ctx, client.ObjectKey{Name: content.Name}, current); err != nil {
			return fmt.Errorf("read PodSnapshotContent %s again: %w", content.Name, err)
		}
		if current.UID != content.UID {
			return fmt.Errorf("PodSnapshotContent %s was replaced", content.Name)
		}
	}
	return fmt.Errorf("record that the member started on PodSnapshotContent %s: the write conflicted %d times", content.Name, readyStatusConflictLimit)
}

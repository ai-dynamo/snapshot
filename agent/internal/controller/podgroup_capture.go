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
	// id is the member's ID, and rank its position in spec.members.
	id   string
	rank int
	// deadline is the PodGroupSnapshot's creation plus spec.deadlineSeconds.
	deadline time.Time
}

// cachedCaptureMember is what captureMemberOf found for one content UID.
type cachedCaptureMember struct {
	uid    ktypes.UID
	member *captureMember
}

// invalidMemberError reports a PodSnapshotContent whose PodSnapshot names a
// pod group it is not a member of.
type invalidMemberError struct{ message string }

func (e *invalidMemberError) Error() string { return e.message }

func invalidMember(format string, args ...any) error {
	return &invalidMemberError{message: fmt.Sprintf(format, args...)}
}

// captureMemberOf returns content's part in a pod group, or nil for a content
// captured on its own. A member's PodSnapshot carries the group labels and is
// controlled by the PodGroupSnapshot they name, whose UID its owner reference
// records; the member's rank is its position in the group's spec.members.
// Without pod groups, every content is captured on its own.
func (w *NodeController) captureMemberOf(ctx context.Context, content *snapshotv1alpha1.PodSnapshotContent) (*captureMember, error) {
	if w.podGroups == nil {
		return nil, nil
	}
	if cached, ok := w.podGroups.captureMembers.Load(content.Name); ok && cached.(cachedCaptureMember).uid == content.UID {
		return cached.(cachedCaptureMember).member, nil
	}
	ref := content.Spec.PodSnapshotRef
	snap := &snapshotv1alpha1.PodSnapshot{}
	if err := w.client.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, snap); err != nil {
		return nil, fmt.Errorf("read PodSnapshot %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	if ref.UID != "" && snap.UID != ref.UID {
		return nil, fmt.Errorf("PodSnapshot %s/%s has UID %s, not %s", ref.Namespace, ref.Name, snap.UID, ref.UID)
	}
	var member *captureMember
	if groupName, grouped := snap.Labels[snapshotv1alpha1.PodGroupLabel]; grouped {
		var err error
		if member, err = w.resolveCaptureMember(ctx, content, snap, groupName); err != nil {
			return nil, err
		}
	}
	w.podGroups.captureMembers.Store(content.Name, cachedCaptureMember{uid: content.UID, member: member})
	return member, nil
}

// resolveCaptureMember reads the PodGroupSnapshot that snap's labels name, and
// checks that it controls snap and names content's source Pod as the member.
func (w *NodeController) resolveCaptureMember(
	ctx context.Context, content *snapshotv1alpha1.PodSnapshotContent, snap *snapshotv1alpha1.PodSnapshot, groupName string,
) (*captureMember, error) {
	id := snap.Labels[snapshotv1alpha1.PodGroupMemberLabel]
	owner := metav1.GetControllerOf(snap)
	if owner == nil || owner.Kind != snapshotv1alpha1.KindPodGroupSnapshot ||
		owner.APIVersion != snapshotv1alpha1.GroupVersion.String() || owner.Name != groupName {
		return nil, invalidMember("PodSnapshot %s/%s is labeled for PodGroupSnapshot %s, which does not control it",
			snap.Namespace, snap.Name, groupName)
	}
	key := client.ObjectKey{Namespace: snap.Namespace, Name: groupName}
	podGroup := &snapshotv1alpha1.PodGroupSnapshot{}
	if err := w.client.Get(ctx, key, podGroup); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, invalidMember("PodGroupSnapshot %s, which controls PodSnapshot %s, is gone", key, snap.Name)
		}
		return nil, fmt.Errorf("read PodGroupSnapshot %s: %w", key, err)
	}
	if podGroup.UID != owner.UID {
		return nil, invalidMember("PodGroupSnapshot %s has UID %s, not %s, which controls PodSnapshot %s",
			key, podGroup.UID, owner.UID, snap.Name)
	}
	rank := slices.IndexFunc(podGroup.Spec.Members, func(m snapshotv1alpha1.PodGroupSnapshotMember) bool { return m.ID == id })
	if rank < 0 {
		return nil, invalidMember("PodGroupSnapshot %s has no member %q", key, id)
	}
	source, captured := podGroup.Spec.Members[rank].Source.PodRef, content.Spec.Source.PodRef
	if source.Name != captured.Name || (source.UID != "" && captured.UID != "" && source.UID != captured.UID) {
		return nil, invalidMember("member %q of PodGroupSnapshot %s is Pod %s, not %s", id, key, source.Name, captured.Name)
	}
	return &captureMember{
		group:    key,
		attempt:  podGroup.UID,
		id:       id,
		rank:     rank,
		deadline: podGroup.CreationTimestamp.Add(time.Duration(podGroup.Spec.DeadlineSeconds) * time.Second),
	}, nil
}

// forgetCaptureMember drops what the agent holds for a content that ended or
// is gone.
func (w *NodeController) forgetCaptureMember(name string) {
	if w.podGroups == nil {
		return
	}
	w.podGroups.captureMembers.Delete(name)
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
//
// There is no second attempt: the session is single-use, and a member that
// fails fails the attempt for every member. Repeats of the dump against the
// suspended workload still spend the capture's retry budget. On success the
// member leaves the attempt off the queue worker.
//
// A failed member reports its own reason, or the attempt's when it failed
// because the attempt ended. A member that never started is left as Snapshot
// found it, without the capture-eligible label; one that started is
// terminated.
func (w *NodeController) runGroupCheckpoint(
	ctx context.Context,
	content *snapshotv1alpha1.PodSnapshotContent,
	pod *corev1.Pod,
	member *captureMember,
	containerName, containerID string,
	containerPID int,
	cuInterposeRequested bool,
	contentUID, artifactPath string,
) error {
	logger := logr.FromContextOrDiscard(ctx).WithValues("attempt", member.attempt, "member", member.id)
	ctx = logr.NewContext(ctx, logger)
	captureCtx, cancel := attemptContext(ctx, member.deadline, w.config.Checkpoint.CheckpointTimeout())
	defer cancel()

	g, err := w.joinAttempt(captureCtx, member.attempt, member.id, w.captureSessionNode(member))
	if err != nil {
		reason, _ := joinFailureReason(err, member.deadline, w.nowFn())
		w.leaveSourceAsFound(ctx, pod)
		return w.failGroupCheckpoint(ctx, logger, content, pod, reason, fmt.Errorf("join the pod group's attempt: %w", err))
	}
	logger.Info("Joined the pod group's attempt", "rank", g.Rank())

	var recorded atomic.Bool
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
	failAttempt(ctx, logger, g)

	failure, err := w.refineFullStaging(ctx, classifyCaptureFailure(err), err, containerPID)
	attempt := settleAttempt(failure, err)
	if recorded.Load() || attempt.suspendedSource {
		w.recoverSourceAfterFailedDump(ctx, logger, pod, containerPID, attempt.suspended, false)
	} else {
		w.leaveSourceAsFound(ctx, pod)
	}
	reason := attempt.failure.reason
	if ended := attemptEndReason(err, member.deadline, w.nowFn()); ended != "" {
		reason = ended
	}
	return w.failGroupCheckpoint(ctx, logger, content, pod, reason, err)
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

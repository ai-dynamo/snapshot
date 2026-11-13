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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	"github.com/ai-dynamo/snapshot/agent/internal/group"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/targetstatus"
	"github.com/ai-dynamo/snapshot/api/compat"
	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// podGroupRestoreGVR is the resource the restore side of pod groups watches.
var podGroupRestoreGVR = snapshotv1alpha1.GroupVersion.WithResource("podgrouprestores")

// uidIndex indexes the PodGroupRestores by UID, which a target's binding names.
const uidIndex = "byUID"

// podGroupRestoreResync re-delivers every PodGroupRestore, the backstop for a
// target whose pin arrived while its Pod was not in the cache yet.
const podGroupRestoreResync = 30 * time.Second

// startPodGroupRestoreInformer watches the PodGroupRestores, cluster-wide, and
// queues a target on this node whenever its PodGroupRestore changes, which is
// how a target learns that it was pinned.
func (w *NodeController) startPodGroupRestoreInformer() (cache.InformerSynced, error) {
	factory := dynamicinformer.NewDynamicSharedInformerFactory(w.dynClient, podGroupRestoreResync)
	informer := factory.ForResource(podGroupRestoreGVR).Informer()
	if err := informer.AddIndexers(cache.Indexers{uidIndex: func(obj any) ([]string, error) {
		accessor, err := metaAccessor(obj)
		if err != nil {
			return nil, nil
		}
		return []string{string(accessor.GetUID())}, nil
	}}); err != nil {
		return nil, fmt.Errorf("index PodGroupRestores by UID: %w", err)
	}
	w.podGroups.restores = informer.GetIndexer()
	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    w.enqueueGroupTargets,
		UpdateFunc: func(_, obj any) { w.enqueueGroupTargets(obj) },
	}); err != nil {
		return nil, fmt.Errorf("add PodGroupRestore informer handler: %w", err)
	}
	go factory.Start(w.stopCh)
	return informer.HasSynced, nil
}

func metaAccessor(obj any) (metav1.Object, error) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, fmt.Errorf("unexpected type %T", obj)
	}
	return u, nil
}

// enqueueGroupTargets queues each target of a PodGroupRestore that runs on
// this node and is bound to it.
func (w *NodeController) enqueueGroupTargets(obj any) {
	restore, ok := podGroupRestoreFromInformerObj(obj)
	if !ok || w.restorePodLister == nil {
		return
	}
	for _, target := range restore.Spec.Targets {
		pod, err := w.restorePodLister.Pods(restore.Namespace).Get(target.PodRef.Name)
		if err != nil || pod.Spec.NodeName != w.config.NodeName ||
			pod.Annotations[podcontract.RestoreGroupUIDAnnotation] != string(restore.UID) {
			continue
		}
		w.restoreQueue.Add(client.ObjectKeyFromObject(pod))
	}
}

func podGroupRestoreFromInformerObj(obj any) (*snapshotv1alpha1.PodGroupRestore, bool) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, false
	}
	restore := &snapshotv1alpha1.PodGroupRestore{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, restore); err != nil {
		return nil, false
	}
	return restore, true
}

// groupTargetRelevant reports whether pod is a claimed group target on this
// node that the agent serves.
func (w *NodeController) groupTargetRelevant(pod *corev1.Pod) bool {
	if w.podGroups == nil || pod.Spec.NodeName != w.config.NodeName {
		return false
	}
	_, bound := pod.Annotations[podcontract.RestoreGroupUIDAnnotation]
	return bound
}

// restoreMember is a claimed group target's part in its PodGroupRestore.
type restoreMember struct {
	restore *snapshotv1alpha1.PodGroupRestore
	claim   podcontract.RestoreGroupClaim
	// id is the ID of the target whose podRef names the Pod: the member of the
	// PodGroupSnapshot it restores.
	id string
	// pin is the member's status once the operator pinned it, or nil.
	pin *snapshotv1alpha1.PodGroupRestoreMemberStatus
	// deadline is the PodGroupRestore's creation plus spec.deadlineSeconds.
	deadline time.Time
}

func (m *restoreMember) attempt() ktypes.UID { return m.restore.UID }

// fence returns the UID that every write on the Pod is fenced on: the pinned
// Pod's, or, before the pin, the UID of the Pod the agent read.
func fence(m *restoreMember, pod *corev1.Pod) ktypes.UID {
	if m != nil && m.pin != nil && m.pin.Pod != nil {
		return m.pin.Pod.UID
	}
	return pod.UID
}

// restoreMemberOf finds the PodGroupRestore that pod's binding names, by UID,
// and the target whose podRef names the Pod. It returns nil while the agent
// has not seen the PodGroupRestore.
func (w *NodeController) restoreMemberOf(pod *corev1.Pod, claim podcontract.RestoreGroupClaim) (*restoreMember, error) {
	objs, err := w.podGroups.restores.ByIndex(uidIndex, string(claim.UID))
	if err != nil {
		return nil, fmt.Errorf("look up PodGroupRestore %s: %w", claim.UID, err)
	}
	var restore *snapshotv1alpha1.PodGroupRestore
	for _, obj := range objs {
		if r, ok := podGroupRestoreFromInformerObj(obj); ok && r.Namespace == pod.Namespace {
			restore = r
		}
	}
	if restore == nil {
		return nil, nil
	}
	i := slices.IndexFunc(restore.Spec.Targets, func(t snapshotv1alpha1.PodGroupRestoreTarget) bool {
		return t.PodRef.Name == pod.Name && (t.PodRef.UID == "" || t.PodRef.UID == pod.UID)
	})
	if i < 0 {
		return nil, fmt.Errorf("PodGroupRestore %s/%s, which claims the Pod, has no target for it", restore.Namespace, restore.Name)
	}
	m := &restoreMember{
		restore:  restore,
		claim:    claim,
		id:       restore.Spec.Targets[i].ID,
		deadline: restore.CreationTimestamp.Add(time.Duration(restore.Spec.DeadlineSeconds) * time.Second),
	}
	for j := range restore.Status.Members {
		if status := &restore.Status.Members[j]; status.ID == m.id && status.Pod != nil {
			m.pin = status
		}
	}
	return m, nil
}

// restoreSessionNode reads the node of the rank-0 member of m's attempt: the
// first member of the PodGroupSnapshot it restores, as its PodGroupRestore's
// status pins it.
func (w *NodeController) restoreSessionNode(m *restoreMember) group.SessionNode {
	return func(ctx context.Context) (string, error) {
		podGroup, err := w.restoredPodGroup(ctx, m)
		if apierrors.IsNotFound(err) || errors.Is(err, errReplacedPodGroup) {
			return "", fmt.Errorf("%w: %w", err, group.ErrNoAttempt)
		}
		if err != nil {
			return "", err
		}
		if len(podGroup.Spec.Members) == 0 {
			return "", nil
		}
		rankZero := podGroup.Spec.Members[0].ID
		objs, err := w.podGroups.restores.ByIndex(uidIndex, string(m.attempt()))
		if err != nil {
			return "", err
		}
		for _, obj := range objs {
			restore, ok := podGroupRestoreFromInformerObj(obj)
			if !ok {
				continue
			}
			for _, status := range restore.Status.Members {
				if status.ID == rankZero {
					return status.NodeName, nil
				}
			}
		}
		return "", nil
	}
}

// restoredPodGroup reads the PodGroupSnapshot that m's PodGroupRestore names,
// and checks its UID. The operator's in-use finalizer keeps it while the
// restore is unfinished, so a PodGroupSnapshot that is gone or replaced fails
// the member. A read that fails otherwise is pending: the target tries again.
func (w *NodeController) restoredPodGroup(ctx context.Context, m *restoreMember) (*snapshotv1alpha1.PodGroupSnapshot, error) {
	ref := m.restore.Spec.SnapshotRef
	key := client.ObjectKey{Namespace: m.restore.Namespace, Name: ref.Name}
	podGroup := &snapshotv1alpha1.PodGroupSnapshot{}
	if err := w.client.Get(ctx, key, podGroup); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("PodGroupSnapshot %s, which PodGroupRestore %s restores, is gone: %w", key, m.restore.Name, err)
		}
		w.log.Error(err, "Failed to read the PodGroupSnapshot of a group restore", "podGroupSnapshot", key.String())
		return nil, newRestorePendingError(reasonGroupPending, fmt.Sprintf("Unable to read PodGroupSnapshot %s; retrying", key))
	}
	if podGroup.UID != ref.UID {
		return nil, fmt.Errorf("PodGroupSnapshot %s has UID %s, not the %s that PodGroupRestore %s names: %w",
			key, podGroup.UID, ref.UID, m.restore.Name, errReplacedPodGroup)
	}
	return podGroup, nil
}

// errReplacedPodGroup reports a PodGroupSnapshot that is not the one a
// PodGroupRestore names.
var errReplacedPodGroup = errors.New("the PodGroupSnapshot was replaced")

// restoreOutcome is the end of a group target's restore, as its report says.
type restoreOutcome struct {
	// fence is the UID the report is fenced on.
	fence  ktypes.UID
	status corev1.ConditionStatus
	// reason is nvidia.com/Restored's, one of the Pod contract's.
	reason, message string
	// groupReason, when set, goes on nvidia.com/GroupRestoreFailed: the
	// session's reason, or one of the agent's group checks.
	groupReason string
}

func succeededOutcome(m *restoreMember, pod *corev1.Pod, message string) *restoreOutcome {
	return &restoreOutcome{fence: fence(m, pod), status: corev1.ConditionTrue, reason: podcontract.RestoreReasonSucceeded, message: message}
}

func failedOutcome(m *restoreMember, pod *corev1.Pod, groupReason string, cause error) *restoreOutcome {
	return &restoreOutcome{fence: fence(m, pod), status: corev1.ConditionFalse, reason: podcontract.RestoreReasonFailed,
		message: cause.Error(), groupReason: groupReason}
}

// groupCheckError is a failure of one of the agent's group checks, which
// goes on nvidia.com/GroupRestoreFailed with its reason.
type groupCheckError struct {
	reason string
	err    error
}

func (e *groupCheckError) Error() string { return e.err.Error() }
func (e *groupCheckError) Unwrap() error { return e.err }

func groupCheckFailed(reason, format string, args ...any) error {
	return &groupCheckError{reason: reason, err: fmt.Errorf(format, args...)}
}

// processGroupRestorePod serves one claimed group target, as
// processRestoreQueueItem does a Pod restored on its own. Every condition it
// writes on the Pod is fenced (see targetstatus). It reports whether the Pod
// should be looked at again after restoreSafetyRequeueInterval.
func (w *NodeController) processGroupRestorePod(ctx context.Context, pod *corev1.Pod, claim podcontract.RestoreGroupClaim, claimErr error) bool {
	log := w.log.WithValues("pod", client.ObjectKeyFromObject(pod).String(), "attempt", claim.UID)
	if claimErr != nil {
		// The operator never writes such a binding, and the NRI plugin refuses
		// to create the Pod's containers, so nothing runs in it.
		log.Error(claimErr, "Restore Pod has an invalid pod group binding")
		emitPodEvent(ctx, w.clientset, log, pod, snapshotEventComponent, corev1.EventTypeWarning,
			podcontract.RestoreReasonFailed, fmt.Sprintf("Invalid pod group binding: %v", claimErr))
		return false
	}
	if w.restoreHandled(pod) {
		return w.removeRestoreFinalizerWithEvent(ctx, pod)
	}
	if outcome := w.pendingRestoreOutcome(pod); outcome != nil {
		return w.finishGroupRestore(ctx, log, pod, outcome) != nil
	}
	if isRestoreTerminal(pod) {
		return w.handleTerminalRestorePod(ctx, pod)
	}
	if pod.DeletionTimestamp != nil {
		return w.removeRestoreFinalizerWithEvent(ctx, pod)
	}
	member, err := w.restoreMemberOf(pod, claim)
	if err != nil {
		return w.finishGroupRestore(ctx, log, pod, failedOutcome(nil, pod, "", err)) != nil
	}
	if member == nil {
		w.reportGroupPending(ctx, log, pod, nil, fmt.Sprintf("Waiting for PodGroupRestore %s, which claims this Pod", claim.UID))
		return false
	}
	log = log.WithValues("member", member.id)
	if restoreStarted(pod) {
		return w.recoverStartedGroupRestore(ctx, log, pod, member)
	}
	if !isRestorePodActive(pod) {
		return w.finishGroupRestore(ctx, log, pod, failedOutcome(member, pod, "",
			fmt.Errorf("pod %s/%s is not eligible for restore in phase %s; expected Pending or Running", pod.Namespace, pod.Name, pod.Status.Phase))) != nil
	}
	if member.pin == nil {
		w.reportGroupPending(ctx, log, pod, member, fmt.Sprintf("Waiting for PodGroupRestore %s to pin this Pod", member.restore.Name))
		return false
	}
	plan, containerID, err := w.preflightGroupRestore(ctx, pod, member)
	if err != nil {
		return w.handleGroupPreflightError(ctx, log, pod, member, err)
	}
	if err := w.addRestoreFinalizer(ctx, pod); err != nil {
		log.Error(err, "Failed to protect restore Pod")
		emitPodEvent(ctx, w.clientset, log, pod, snapshotEventComponent, corev1.EventTypeWarning, restoreFinalizerUpdateFailedReason, err.Error())
		return true
	}
	return w.runGroupRestore(ctx, log, pod, member, plan, containerID)
}

// reportGroupPending reports, on nvidia.com/Restored, that the target waits
// for its group. A report already on the Pod is not written again.
func (w *NodeController) reportGroupPending(ctx context.Context, log logr.Logger, pod *corev1.Pod, m *restoreMember, message string) {
	if c := findRestoredCondition(pod); c != nil && c.Status == corev1.ConditionFalse && c.Reason == reasonGroupPending && c.Message == message {
		return
	}
	_, err := targetstatus.Apply(ctx, w.clientset, pod, fence(m, pod), targetstatus.RestoreFieldManager, corev1.PodCondition{
		Type: podcontract.RestoredCondition, Status: corev1.ConditionFalse, Reason: reasonGroupPending, Message: message,
	})
	if err != nil {
		log.Error(err, "Failed to report that the restore waits for its pod group")
		return
	}
	log.V(1).Info("Restore waits for its pod group", "message", message)
}

// preflightGroupRestore checks a pinned target before it joins its attempt:
// the Pod and the container the NRI plugin prepared are the ones the operator
// pinned, the member's artifact resolves and passes the compatibility gates,
// and the destination's startup probe leaves the restore its deadline. It
// returns the plan and the destination container's ID.
func (w *NodeController) preflightGroupRestore(ctx context.Context, pod *corev1.Pod, m *restoreMember) (*restorePlan, string, error) {
	destination := m.claim.Mapping.Destination
	containerID, err := w.checkGroupPin(pod, m)
	if err != nil {
		return nil, "", err
	}
	if err := podcontract.ValidateGroupTarget(pod, podcontract.GroupTargetRequest{DestinationContainer: destination}); err != nil {
		return nil, "", err
	}
	content, target, err := w.groupRestoreContent(ctx, pod, m)
	if err != nil {
		return nil, "", err
	}
	mappings := []podcontract.ContainerMapping{m.claim.Mapping}
	artifact, err := w.resolveRestoreArtifact(client.ObjectKeyFromObject(pod).String(), target, content)
	if err != nil {
		return nil, "", err
	}
	skipCompatCheck := w.skipCompatCheckRequested(pod)
	if err := w.preflightCompatibility(ctx, pod, artifact, mappings, skipCompatCheck); err != nil {
		return nil, "", err
	}
	if err := checkStartupBudget(pod, destination, m.deadline, w.nowFn()); err != nil {
		return nil, "", err
	}
	postRestore, postRestorePeriod, err := planPostRestore(pod, mappings,
		postRestoreAction(content, target.SourceContainerName), w.hookExec, w.hookHTTP)
	if err != nil {
		return nil, "", err
	}
	return &restorePlan{
		artifact:          artifact,
		mappings:          mappings,
		skipCompatCheck:   skipCompatCheck,
		postRestore:       postRestore,
		postRestorePeriod: postRestorePeriod,
	}, containerID, nil
}

// checkGroupPin checks that the Pod, its IP and its destination container are
// the ones the operator pinned at admission. The agent restores only into the
// container it was pinned to, so a Pod or a container that changed since
// fails the member with PodIPChanged rather than restore with a stale map.
func (w *NodeController) checkGroupPin(pod *corev1.Pod, m *restoreMember) (string, error) {
	pin, destination := m.pin, m.claim.Mapping.Destination
	if pin.Pod.UID != pod.UID {
		return "", groupCheckFailed(snapshotv1alpha1.PodGroupReasonPodIPChanged,
			"the restore Pod is %s, not the pinned %s", pod.UID, pin.Pod.UID)
	}
	if pod.Status.PodIP != pin.TargetPodIP {
		return "", groupCheckFailed(snapshotv1alpha1.PodGroupReasonPodIPChanged,
			"the restore Pod's IP is %q, not the pinned %q", pod.Status.PodIP, pin.TargetPodIP)
	}
	i := slices.IndexFunc(pin.Containers, func(c snapshotv1alpha1.PodGroupRestoreContainerStatus) bool { return c.Name == destination })
	if i < 0 {
		return "", groupCheckFailed(snapshotv1alpha1.PodGroupReasonPodIPChanged,
			"PodGroupRestore %s pins no container %q", m.restore.Name, destination)
	}
	pinned := snapshotruntime.StripCRIScheme(pin.Containers[i].ContainerID)
	current := restoreContainerIDFromStatus(pod, destination)
	if current == "" {
		if started, ok := w.startedRestoreContainers.Load(restoreContainerKey(pod.UID, destination)); ok {
			current = started.(string)
		}
	}
	switch {
	case current == "":
		return "", newRestorePendingError(reasonGroupPending, fmt.Sprintf("Waiting for container %s to run", destination))
	case current != pinned:
		return "", groupCheckFailed(snapshotv1alpha1.PodGroupReasonPodIPChanged,
			"container %q is %s, not the pinned %s", destination, current, pinned)
	}
	return current, nil
}

// groupRestoreContent resolves the member's artifact: the PodGroupSnapshot
// that the PodGroupRestore names, the member's PodSnapshot in its status, and
// that snapshot's content. The source container must be the one the target's
// mapping names.
func (w *NodeController) groupRestoreContent(ctx context.Context, pod *corev1.Pod, m *restoreMember) (*snapshotv1alpha1.PodSnapshotContent, *restoreTarget, error) {
	podGroup, err := w.restoredPodGroup(ctx, m)
	if err != nil {
		return nil, nil, err
	}
	i := slices.IndexFunc(podGroup.Status.Members, func(s snapshotv1alpha1.PodGroupSnapshotMemberStatus) bool {
		return s.ID == m.id && s.PodSnapshot != nil
	})
	if i < 0 {
		return nil, nil, fmt.Errorf("PodGroupSnapshot %s has no PodSnapshot for member %q", podGroup.Name, m.id)
	}
	ref := podGroup.Status.Members[i].PodSnapshot
	snap := &snapshotv1alpha1.PodSnapshot{}
	key := client.ObjectKey{Namespace: podGroup.Namespace, Name: ref.Name}
	if err := w.client.Get(ctx, key, snap); err != nil {
		// The PodGroupSnapshot owns its members' PodSnapshots, and its in-use
		// finalizer keeps them while the restore is unfinished.
		if apierrors.IsNotFound(err) {
			return nil, nil, fmt.Errorf("PodSnapshot %s of member %q is gone: %w", key, m.id, err)
		}
		w.log.Error(err, "Failed to read the PodSnapshot of a group restore", "podSnapshot", key.String(), "member", m.id)
		return nil, nil, newRestorePendingError(reasonGroupPending, fmt.Sprintf("Unable to read PodSnapshot %s of member %s; retrying", key, m.id))
	}
	if snap.UID != ref.UID {
		return nil, nil, fmt.Errorf("PodSnapshot %s/%s has UID %s, not the pinned %s", snap.Namespace, snap.Name, snap.UID, ref.UID)
	}
	if err := validatePodSnapshotForRestore(snap); err != nil {
		return nil, nil, err
	}
	content, err := w.getPodSnapshotContentFromSnapshot(ctx, pod, snap)
	if err != nil {
		return nil, nil, err
	}
	if err := validatePodSnapshotContentForRestore(content); err != nil {
		return nil, nil, err
	}
	source, err := singleTargetContainer(content.Spec.Source.PodRef.Containers)
	if err != nil {
		return nil, nil, err
	}
	if m.claim.Mapping.Source != source {
		return nil, nil, fmt.Errorf("the target maps container %q, but member %q checkpointed %q", m.claim.Mapping.Source, m.id, source)
	}
	return content, &restoreTarget{SnapshotName: snap.Name, ContentUID: string(content.UID), SourceContainerName: source}, nil
}

// checkStartupBudget refuses a restore whose destination's startup probe
// gives up before the restore's deadline: the time left in the probe must
// cover the time left before the deadline, or the kubelet could restart the
// container while its peers still wait for it.
func checkStartupBudget(pod *corev1.Pod, destination string, deadline, now time.Time) error {
	i := slices.IndexFunc(pod.Spec.Containers, func(c corev1.Container) bool { return c.Name == destination })
	if i < 0 || pod.Spec.Containers[i].StartupProbe == nil {
		return nil
	}
	probe := pod.Spec.Containers[i].StartupProbe
	period, threshold := probe.PeriodSeconds, probe.FailureThreshold
	if period <= 0 {
		period = 10
	}
	if threshold <= 0 {
		threshold = 3
	}
	started := now
	if status := containerStatus(pod, destination); status != nil && status.State.Running != nil && !status.State.Running.StartedAt.IsZero() {
		started = status.State.Running.StartedAt.Time
	}
	givesUp := started.Add(time.Duration(probe.InitialDelaySeconds+period*threshold) * time.Second)
	if givesUp.Before(deadline) {
		return groupCheckFailed(snapshotv1alpha1.PodGroupReasonGateBudgetTooShort,
			"the startup probe of container %q gives up in %s, before the restore's deadline in %s",
			destination, givesUp.Sub(now).Round(time.Second), deadline.Sub(now).Round(time.Second))
	}
	return nil
}

// handleGroupPreflightError reports a target that cannot restore yet, or at
// all. A dependency that is not ready leaves it waiting. Anything else fails
// the member, which refuses the attempt so its peers stop waiting for it.
func (w *NodeController) handleGroupPreflightError(ctx context.Context, log logr.Logger, pod *corev1.Pod, m *restoreMember, cause error) bool {
	var pending *restorePendingError
	if errors.As(cause, &pending) {
		w.reportGroupPending(ctx, log, pod, m, pending.message)
		return true
	}
	w.refuseAttempt(ctx, log, m)
	var incompatible *compat.IncompatibleError
	if errors.As(cause, &incompatible) {
		reason := compat.Reasons(incompatible.Mismatches)
		w.logRestoreRefusal(pod, incompatible, reason)
		return w.finishGroupRestore(ctx, log, pod, &restoreOutcome{fence: fence(m, pod), status: corev1.ConditionFalse,
			reason: podcontract.RestoreReasonIncompatible, message: refusalMessage(reason)}) != nil
	}
	groupReason := ""
	var check *groupCheckError
	if errors.As(cause, &check) {
		groupReason = check.reason
	}
	return w.finishGroupRestore(ctx, log, pod, failedOutcome(m, pod, groupReason, cause)) != nil
}

// refuseAttempt fails m's attempt, before it commits, for a member that
// cannot restore: it joins, and fails the attempt with group.ReasonRefused, so
// its peers stop waiting at admission. It runs off the queue worker, and only
// as long as groupFailTimeout allows; the attempt's deadline ends it anyway.
func (w *NodeController) refuseAttempt(ctx context.Context, log logr.Logger, m *restoreMember) {
	w.podGroups.leaving.Add(1)
	go func() {
		defer w.podGroups.leaving.Done()
		refuseCtx, cancel := context.WithTimeout(ctx, groupFailTimeout)
		defer cancel()
		g, err := w.joinAttempt(refuseCtx, m.attempt(), m.id, w.restoreSessionNode(m))
		if err != nil {
			log.Info("Cannot join the pod group's attempt to refuse it; its deadline ends it", "error", err.Error())
			return
		}
		failAttempt(refuseCtx, log, g)
	}()
}

// groupRestoreRun is a group target's part in its attempt, for the executor.
type groupRestoreRun struct {
	member  *group.Member
	attempt ktypes.UID
	// committed records that the member started, once the attempt commits;
	// recorded is set once the record is written.
	committed func(context.Context) error
	recorded  atomic.Bool
}

// runGroupRestore restores a pinned target as its member's part in the
// group's restore. It joins the attempt and runs the restore; the attempt
// commits at the admitted barrier, and the member records that it started,
// with the fenced nvidia.com/RestoreStarted condition, before its first
// irreversible step. After the release barrier it writes restore-complete with
// the attempt's UID, leaves the attempt off the queue worker, and runs the
// member's post-restore action.
//
// A member that never started is left as Snapshot found it; one that started
// is failed, which ends its restored processes. The report carries the
// member's own failure, or the attempt's reason when the attempt ended, which
// also goes on nvidia.com/GroupRestoreFailed.
func (w *NodeController) runGroupRestore(ctx context.Context, log logr.Logger, pod *corev1.Pod, m *restoreMember, plan *restorePlan, containerID string) bool {
	restoreCtx, cancel := attemptContext(ctx, m.deadline, w.config.Restore.RestoreTimeout())
	defer cancel()
	destination := m.claim.Mapping.Destination

	g, err := w.joinAttempt(restoreCtx, m.attempt(), m.id, w.restoreSessionNode(m))
	if err != nil {
		reason, ofGroup := joinFailureReason(err, m.deadline, w.nowFn())
		groupReason := ""
		if ofGroup {
			groupReason = reason
		}
		return w.finishGroupRestore(ctx, log, pod, failedOutcome(m, pod, groupReason,
			fmt.Errorf("join the pod group's attempt (%s): %w", reason, err))) != nil
	}
	log.Info("Joined the pod group's attempt", "rank", g.Rank())

	current := pod
	op := w.newRestoreOperation(pod, plan, destination, containerID, time.Now())
	run := &groupRestoreRun{member: g, attempt: m.attempt()}
	run.committed = func(ctx context.Context) error {
		started, err := targetstatus.Apply(ctx, w.clientset, current, fence(m, pod), targetstatus.StartedFieldManager, corev1.PodCondition{
			Type: podcontract.RestoreStartedCondition, Status: corev1.ConditionTrue, Reason: "RestoreStarted",
			Message: "Every member of the pod group passed admission; restoring this member",
		})
		if err != nil {
			return fmt.Errorf("record that the member started: %w", err)
		}
		run.recorded.Store(true)
		current = started
		inProgress, err := targetstatus.Apply(ctx, w.clientset, current, fence(m, pod), targetstatus.RestoreFieldManager, corev1.PodCondition{
			Type: podcontract.RestoredCondition, Status: corev1.ConditionFalse, Reason: podcontract.RestoreReasonInProgress,
			Message: fmt.Sprintf("Restoring member %s of PodGroupRestore %s from PodSnapshot %s", m.id, m.restore.Name, plan.artifact.SnapshotName),
		})
		if err != nil {
			return fmt.Errorf("report the restore in progress: %w", err)
		}
		current = inProgress
		return nil
	}
	op.group = run
	emitPodEvent(ctx, w.clientset, log, pod, snapshotEventComponent, corev1.EventTypeNormal, restoreRequestedReason,
		fmt.Sprintf("Restore requested from PodSnapshot %s for destination %s, as member %s of PodGroupRestore %s",
			plan.artifact.SnapshotName, destination, m.id, m.restore.Name))

	outcome := w.restoreGroupMember(ctx, restoreCtx, log, current, m, op, run)
	if outcome == nil {
		return false
	}
	return w.finishGroupRestore(ctx, log, current, outcome) != nil
}

// restoreGroupMember runs the executor for one group target and returns the
// outcome to report, or nil when the agent is shutting down mid-way through
// the post-restore action, which the next agent resumes.
func (w *NodeController) restoreGroupMember(
	ctx, restoreCtx context.Context, log logr.Logger, pod *corev1.Pod, m *restoreMember, op *restoreOperation, run *groupRestoreRun,
) (outcome *restoreOutcome) {
	// As runRestore does: without this a started member's placeholder keeps
	// waiting for a restore that panicked.
	defer recoverPanic(op.log, "group restore", func(err error) {
		failAttempt(ctx, log, run.member)
		if run.recorded.Load() {
			err = op.failRestore(ctx, err)
		}
		outcome = failedOutcome(m, pod, "", err)
	}, "destination", op.destination)
	restored, err := op.executeRestore(restoreCtx)
	if err != nil {
		var incompatible *compat.IncompatibleError
		if errors.As(err, &incompatible) {
			// Refused before the commit: the placeholder is left running.
			reason := compat.Reasons(incompatible.Mismatches)
			w.logRestoreRefusal(pod, incompatible, reason)
			return &restoreOutcome{fence: fence(m, pod), status: corev1.ConditionFalse,
				reason: podcontract.RestoreReasonIncompatible, message: refusalMessage(reason)}
		}
		var cleanupErr *executor.RestoreCleanupError
		if !errors.As(err, &cleanupErr) {
			failAttempt(ctx, log, run.member)
			if run.recorded.Load() {
				err = op.failRestore(ctx, err)
			} else {
				logWithStep(op.log, err).Error(err, "Restore failed before the member started; leaving the Pod as found")
			}
			return failedOutcome(m, pod, attemptEndReason(err, m.deadline, w.nowFn()), err)
		}
		logWithStep(op.log, err).Error(cleanupErr, "Restore completed with cleanup errors")
		emitPodEvent(ctx, w.clientset, op.log, pod, snapshotEventComponent, corev1.EventTypeWarning, "RestoreCleanupFailed", cleanupErr.Error())
	}
	if err := op.completeRestore(ctx, restored); err != nil {
		failAttempt(ctx, log, run.member)
		return failedOutcome(m, pod, "", err)
	}
	w.leaveAttempt(ctx, log, run.member)
	if err := op.notifyPostRestore(restoreCtx); err != nil {
		if err := op.failPostRestore(ctx, err); errors.Is(err, errRestoreInterrupted) {
			return nil
		} else if err != nil {
			return failedOutcome(m, pod, "", err)
		}
	}
	return succeededOutcome(m, pod, fmt.Sprintf("Restored member %s of PodGroupRestore %s into destination container %s",
		m.id, m.restore.Name, m.claim.Mapping.Destination))
}

// recoverStartedGroupRestore serves a target that had started when this agent
// restarted. CRIU may already have run into it, so it never runs again: a
// target whose restore-complete exists was released and keeps running, and at
// most its post-restore action is left; any other fails.
func (w *NodeController) recoverStartedGroupRestore(ctx context.Context, log logr.Logger, pod *corev1.Pod, m *restoreMember) bool {
	destination := m.claim.Mapping.Destination
	containerID := restoreContainerIDFromStatus(pod, destination)
	plan := &restorePlan{artifact: &restoreArtifact{}, mappings: []podcontract.ContainerMapping{m.claim.Mapping}}
	content, target, err := w.groupRestoreContent(ctx, pod, m)
	var pending *restorePendingError
	if errors.As(err, &pending) {
		// A released member's post-restore action needs its content, so a
		// read that failed is retried rather than skipping the action.
		log.Info("Cannot read the interrupted member's snapshot yet; checking again", "reason", pending.message)
		return true
	}
	if err == nil {
		plan.artifact = &restoreArtifact{SnapshotName: target.SnapshotName, ContentUID: target.ContentUID, SourceContainerName: target.SourceContainerName}
		if plan.postRestore, plan.postRestorePeriod, err = planPostRestore(pod, plan.mappings,
			postRestoreAction(content, target.SourceContainerName), w.hookExec, w.hookHTTP); err != nil {
			log.Error(err, "Cannot plan the interrupted member's post-restore action")
		}
	}
	op := w.newRestoreOperation(pod, plan, destination, containerID, time.Now())
	released := false
	if containerID != "" {
		hostPID, _, err := w.runtime.ResolveContainer(ctx, containerID)
		if err == nil {
			released, err = w.controlSentinelExistsFn(hostPID, podcontract.RestoreCompleteFile)
		}
		if err != nil {
			log.Error(err, "Cannot tell whether the interrupted member was released; checking again")
			return true
		}
	}
	if !released {
		cause := errors.New("the agent restarted after the member started, before its release; a member that started is never restored again")
		emitPodEvent(ctx, w.clientset, log, pod, snapshotEventComponent, corev1.EventTypeWarning, reasonRestoreInterrupted, cause.Error())
		return w.finishGroupRestore(ctx, log, pod, failedOutcome(m, pod, "", op.failRestore(ctx, cause))) != nil
	}
	if err := op.recoverPostRestore(ctx); err != nil {
		if errors.Is(err, errRestoreInterrupted) {
			return false
		}
		return w.finishGroupRestore(ctx, log, pod, failedOutcome(m, pod, "", err)) != nil
	}
	return w.finishGroupRestore(ctx, log, pod, succeededOutcome(m, pod,
		fmt.Sprintf("Restored member %s of PodGroupRestore %s into destination container %s", m.id, m.restore.Name, destination))) != nil
}

// pendingRestoreOutcome returns the outcome this agent holds for pod, or nil.
func (w *NodeController) pendingRestoreOutcome(pod *corev1.Pod) *restoreOutcome {
	held, ok := w.podGroups.restoreOutcomes.Load(pod.UID)
	if !ok {
		return nil
	}
	return held.(*restoreOutcome)
}

// finishGroupRestore writes a group target's final report, fenced, and then
// does what finishRestore does: it marks the Pod handled, tells its owner,
// and lifts the restore finalizer. The outcome is kept until the Pod is gone,
// so a report that does not land is sent again rather than the member
// restored again. A Pod that is gone or was replaced has nothing to report on.
func (w *NodeController) finishGroupRestore(ctx context.Context, log logr.Logger, pod *corev1.Pod, outcome *restoreOutcome) error {
	w.podGroups.restoreOutcomes.Store(pod.UID, outcome)
	conditions := []corev1.PodCondition{{
		Type: podcontract.RestoredCondition, Status: outcome.status, Reason: outcome.reason, Message: outcome.message,
	}}
	if outcome.groupReason != "" {
		conditions = append(conditions, corev1.PodCondition{
			Type: podcontract.GroupRestoreFailedCondition, Status: corev1.ConditionTrue, Reason: outcome.groupReason, Message: outcome.message,
		})
	}
	_, err := targetstatus.Apply(ctx, w.clientset, pod, outcome.fence, targetstatus.RestoreFieldManager, conditions...)
	switch {
	case errors.Is(err, targetstatus.ErrReplaced) || apierrors.IsNotFound(err):
		log.Info("The restore Pod is gone or was replaced; nothing to report on", "reason", outcome.reason)
		w.markRestoreHandled(pod)
		return nil
	case err != nil:
		emitPodEvent(ctx, w.clientset, log, pod, snapshotEventComponent, corev1.EventTypeWarning, restoreStatusUpdateFailedReason,
			fmt.Sprintf("Failed to record %s restore status: %v", outcome.reason, err))
		return err
	}
	w.markRestoreHandled(pod)
	eventType, reason := corev1.EventTypeWarning, outcome.reason
	if outcome.status == corev1.ConditionTrue {
		eventType = corev1.EventTypeNormal
	}
	if outcome.groupReason != "" {
		reason = outcome.groupReason
	}
	emitPodEvent(ctx, w.clientset, log, pod, snapshotEventComponent, eventType, reason, outcome.message)
	if err := w.removeRestoreFinalizer(ctx, pod); err != nil {
		log.Error(err, "Failed to remove restore protection finalizer")
		emitPodEvent(ctx, w.clientset, log, pod, snapshotEventComponent, corev1.EventTypeWarning, restoreFinalizerUpdateFailedReason, err.Error())
		return err
	}
	return nil
}

// failGroupRestoreAfterPanic reports a panic on a group target, fenced.
func (w *NodeController) failGroupRestoreAfterPanic(ctx context.Context, pod *corev1.Pod, cause error) {
	var m *restoreMember
	if claim, _, err := podcontract.RestoreGroupClaimOf(pod.Annotations); err == nil {
		m, _ = w.restoreMemberOf(pod, claim)
	}
	for range panicStatusRetries {
		if err := w.finishGroupRestore(ctx, w.log, pod, failedOutcome(m, pod, "", cause)); err == nil {
			return
		}
	}
}

// restoreStarted reports whether a group target records that its member
// started.
func restoreStarted(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == podcontract.RestoreStartedCondition && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// forgetGroupTarget drops what the agent holds for a restore Pod that is gone.
func (w *NodeController) forgetGroupTarget(uid ktypes.UID) {
	if w.podGroups != nil {
		w.podGroups.restoreOutcomes.Delete(uid)
	}
}

// groupAttempt is the attempt a restore releases its member in, written to
// restore-complete, or "" for a Pod restored on its own.
func (op *restoreOperation) groupAttempt() ktypes.UID {
	if op.group == nil {
		return ""
	}
	return op.group.attempt
}

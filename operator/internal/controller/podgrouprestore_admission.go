// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// admit decides whether the operator may pin every member, reading through
// reader, and returns what to pin for each spec target when it may. Every
// refusal is checked before any wait, so a restore that can never pass is
// refused at once rather than after its Pods or agents are ready:
//
//  1. the PodGroupSnapshot exists with the pinned UID and is not being
//     deleted (else Refused), and is Ready (else ArtifactUnavailable): a
//     restore never waits for its snapshot;
//  2. the targets match the snapshot's members one to one by ID, name
//     distinct Pods, and map each member's own container
//     (podGroupRestoreSpecRefusal);
//  3. each target's verdict (targetVerdict): any refusal refuses the restore,
//     and any wait makes it wait with TargetPodPending;
//  4. the agent on every target's node is ready for groups, or admission
//     waits (AgentNotReady).
func (r *PodGroupRestoreReconciler) admit(
	ctx context.Context,
	reader client.Reader,
	restore *snapshotv1alpha1.PodGroupRestore,
) (podGroupAdmission, []snapshotv1alpha1.PodGroupRestoreMemberStatus, error) {
	snapshot, refusal, err := admitPodGroupRestoreSnapshot(ctx, reader, restore)
	if err != nil {
		return podGroupAdmission{}, nil, err
	}
	if refusal != nil {
		return *refusal, nil, nil
	}
	if refusal := podGroupRestoreSpecRefusal(restore, snapshot); refusal != nil {
		return *refusal, nil, nil
	}
	pinnedBy, err := podGroupRestorePins(ctx, reader, restore)
	if err != nil {
		return podGroupAdmission{}, nil, err
	}
	members := podGroupSnapshotMembersByID(snapshot)

	var pending []string
	pins := make([]snapshotv1alpha1.PodGroupRestoreMemberStatus, len(restore.Spec.Targets))
	nodes := make(map[string]struct{}, len(restore.Spec.Targets))
	for i := range restore.Spec.Targets {
		target := &restore.Spec.Targets[i]
		verdict, err := targetVerdict(ctx, reader, restore, target, members[target.ID], pinnedBy)
		switch {
		case err != nil:
			return podGroupAdmission{}, nil, err
		case verdict.refusal != nil:
			return *verdict.refusal, nil, nil
		case verdict.pending != "":
			pending = append(pending, verdict.pending)
		default:
			pins[i] = *verdict.pin
			nodes[verdict.pin.NodeName] = struct{}{}
		}
	}

	if len(pending) > 0 {
		return podGroupAdmission{podGroupAdmissionWaiting, snapshotv1alpha1.PodGroupReasonTargetPodPending, strings.Join(pending, "; ")}, nil, nil
	}
	wait, err := waitForPodGroupAgents(ctx, reader, r.AgentNamespace, r.AgentSelector, nodes)
	if err != nil {
		return podGroupAdmission{}, nil, err
	}
	if wait != nil {
		return *wait, nil, nil
	}
	return podGroupAdmission{state: podGroupAdmissionPassed}, pins, nil
}

// admitPodGroupRestoreSnapshot reads the restore's PodGroupSnapshot, and
// returns it, or a refusal: Refused when it is missing, another object, or
// being deleted, and ArtifactUnavailable when it is not Ready.
func admitPodGroupRestoreSnapshot(
	ctx context.Context,
	reader client.Reader,
	restore *snapshotv1alpha1.PodGroupRestore,
) (*snapshotv1alpha1.PodGroupSnapshot, *podGroupAdmission, error) {
	ref := restore.Spec.SnapshotRef
	snapshot := &snapshotv1alpha1.PodGroupSnapshot{}
	switch err := reader.Get(ctx, client.ObjectKey{Namespace: restore.Namespace, Name: ref.Name}, snapshot); {
	case apierrors.IsNotFound(err):
		return nil, ptr.To(podGroupRefusal(fmt.Sprintf("PodGroupSnapshot %q not found", ref.Name))), nil
	case err != nil:
		return nil, nil, fmt.Errorf("get PodGroupSnapshot %q: %w", ref.Name, err)
	case snapshot.UID != ref.UID:
		return nil, ptr.To(podGroupRefusal(fmt.Sprintf("PodGroupSnapshot %q has UID %q, not %q: it was recreated",
			ref.Name, snapshot.UID, ref.UID))), nil
	case !snapshot.DeletionTimestamp.IsZero():
		return nil, ptr.To(podGroupRefusal(fmt.Sprintf("PodGroupSnapshot %q is being deleted", ref.Name))), nil
	}
	if meta.IsStatusConditionTrue(snapshot.Status.Conditions, snapshotv1alpha1.PodGroupConditionReady) {
		return snapshot, nil, nil
	}
	message := fmt.Sprintf("PodGroupSnapshot %q is not Ready", ref.Name)
	if ready := meta.FindStatusCondition(snapshot.Status.Conditions, snapshotv1alpha1.PodGroupConditionReady); ready != nil {
		message += ": " + ready.Message
	}
	return nil, &podGroupAdmission{podGroupAdmissionRefused, snapshotv1alpha1.PodGroupReasonArtifactUnavailable, message}, nil
}

// podGroupRestoreSpecRefusal makes the checks of the restore's spec against
// its PodGroupSnapshot, and returns the first refusal, or nil:
//
//   - every target has a member with its ID, and every member a target;
//   - targets name distinct Pods: the CRD cannot check it, as for members;
//   - a target's container mapping, when it has one, maps the container its
//     member was checkpointed from.
func podGroupRestoreSpecRefusal(restore *snapshotv1alpha1.PodGroupRestore, snapshot *snapshotv1alpha1.PodGroupSnapshot) *podGroupAdmission {
	members := podGroupSnapshotMembersByID(snapshot)
	targets := make(map[string]struct{}, len(restore.Spec.Targets))
	for _, target := range restore.Spec.Targets {
		if _, ok := members[target.ID]; !ok {
			return ptr.To(podGroupRefusal(fmt.Sprintf("target %q matches no member of PodGroupSnapshot %q", target.ID, snapshot.Name)))
		}
		targets[target.ID] = struct{}{}
	}
	for _, member := range snapshot.Spec.Members {
		if _, ok := targets[member.ID]; !ok {
			return ptr.To(podGroupRefusal(fmt.Sprintf("member %q of PodGroupSnapshot %q has no target", member.ID, snapshot.Name)))
		}
	}

	pods := make(map[string]string, len(restore.Spec.Targets))
	for _, target := range restore.Spec.Targets {
		if first, found := pods[target.PodRef.Name]; found {
			return ptr.To(podGroupRefusal(fmt.Sprintf("targets %q and %q name the same Pod %q", first, target.ID, target.PodRef.Name)))
		}
		pods[target.PodRef.Name] = target.ID
	}

	for _, target := range restore.Spec.Targets {
		source := members[target.ID].Source.PodRef.Containers[0]
		for _, mapping := range target.ContainerMappings {
			if mapping.Source != source {
				return ptr.To(podGroupRefusal(fmt.Sprintf("target %q maps container %q, but member %q was checkpointed from container %q",
					target.ID, mapping.Source, target.ID, source)))
			}
		}
	}
	return nil
}

// podGroupSnapshotMembersByID maps each member of a PodGroupSnapshot by its ID.
func podGroupSnapshotMembersByID(snapshot *snapshotv1alpha1.PodGroupSnapshot) map[string]*snapshotv1alpha1.PodGroupSnapshotMember {
	members := make(map[string]*snapshotv1alpha1.PodGroupSnapshotMember, len(snapshot.Spec.Members))
	for i := range snapshot.Spec.Members {
		members[snapshot.Spec.Members[i].ID] = &snapshot.Spec.Members[i]
	}
	return members
}

// podGroupRestoreDestination is the restore Pod's container that receives a
// target's member: its mapping's destination, or the member's own container
// when the target maps none.
func podGroupRestoreDestination(target *snapshotv1alpha1.PodGroupRestoreTarget, member *snapshotv1alpha1.PodGroupSnapshotMember) string {
	if len(target.ContainerMappings) > 0 {
		return target.ContainerMappings[0].Destination
	}
	return member.Source.PodRef.Containers[0]
}

// podGroupRestorePins maps the UID of every Pod that an unfinished
// PodGroupRestore other than restore has pinned to that restore's name. It
// lists the namespace's PodGroupRestores once, through reader, which is the
// API server when admission confirms its result, and only reads them.
func podGroupRestorePins(ctx context.Context, reader client.Reader, restore *snapshotv1alpha1.PodGroupRestore) (map[types.UID]string, error) {
	restores := &snapshotv1alpha1.PodGroupRestoreList{}
	if err := reader.List(ctx, restores, client.InNamespace(restore.Namespace), client.UnsafeDisableDeepCopy); err != nil {
		return nil, fmt.Errorf("list PodGroupRestores: %w", err)
	}
	pinnedBy := make(map[types.UID]string)
	for i := range restores.Items {
		other := &restores.Items[i]
		if other.UID == restore.UID || podGroupRestoreFinished(&other.Status) {
			continue
		}
		for _, m := range other.Status.Members {
			if m.Pod != nil {
				pinnedBy[m.Pod.UID] = other.Name
			}
		}
	}
	return pinnedBy, nil
}

// podGroupRestoreTargetVerdict is admission's verdict on one target: exactly
// one of its fields is set.
type podGroupRestoreTargetVerdict struct {
	// refusal refuses the restore: the target can never pass.
	refusal *podGroupAdmission
	// pending says what the target waits for.
	pending string
	// pin is what to pin for a target that is ready.
	pin *snapshotv1alpha1.PodGroupRestoreMemberStatus
}

// targetVerdict decides one target's verdict, reading its Pod through reader,
// for member, the snapshot member with the target's ID. pinnedBy maps the
// Pods that other unfinished restores pinned to their names. Every refusal
// comes before any wait, and TargetInUse before Refused, so a Pod another
// attempt still uses is reported as in use. The first matching step decides:
//
//  1. the Pod does not exist yet: it waits;
//  2. its UID is not the one the spec pins, if any: Refused;
//  3. it has no nvidia.com/restore-group: it is no group target, Refused;
//  4. its binding, read by podcontract.RestoreGroupBinding, binds it, by the
//     exact name and UID, to another restore that exists and has not
//     finished: TargetInUse;
//  5. another unfinished restore pinned it: TargetInUse;
//  6. it carries nvidia.com/RestoreStarted, of any status and from any
//     attempt: an earlier restore changed it, Refused;
//  7. its address is not IPv4, or it runs without the destination container:
//     Refused;
//  8. its binding is not this restore's exact name and UID: it waits to be
//     bound, since its binding has no UID, or names an attempt that has
//     finished, no longer exists, or was recreated;
//  9. it is not scheduled, has no address, or its destination container has
//     no ID yet: it waits;
//  10. otherwise it is ready, with its pin.
func targetVerdict(
	ctx context.Context,
	reader client.Reader,
	restore *snapshotv1alpha1.PodGroupRestore,
	target *snapshotv1alpha1.PodGroupRestoreTarget,
	member *snapshotv1alpha1.PodGroupSnapshotMember,
	pinnedBy map[types.UID]string,
) (podGroupRestoreTargetVerdict, error) {
	pod := &corev1.Pod{}
	switch err := reader.Get(ctx, client.ObjectKey{Namespace: restore.Namespace, Name: target.PodRef.Name}, pod); {
	case apierrors.IsNotFound(err):
		return podGroupRestoreTargetVerdict{pending: fmt.Sprintf("target %q: Pod %q does not exist yet", target.ID, target.PodRef.Name)}, nil
	case err != nil:
		return podGroupRestoreTargetVerdict{}, fmt.Errorf("get Pod %q of target %q: %w", target.PodRef.Name, target.ID, err)
	}
	about := fmt.Sprintf("target %q: Pod %q ", target.ID, pod.Name)
	refuse := func(reason, format string, args ...any) (podGroupRestoreTargetVerdict, error) {
		return podGroupRestoreTargetVerdict{refusal: &podGroupAdmission{podGroupAdmissionRefused, reason, about + fmt.Sprintf(format, args...)}}, nil
	}
	wait := func(format string, args ...any) (podGroupRestoreTargetVerdict, error) {
		return podGroupRestoreTargetVerdict{pending: about + fmt.Sprintf(format, args...)}, nil
	}

	if uid := target.PodRef.UID; uid != "" && pod.UID != uid {
		return refuse(snapshotv1alpha1.PodGroupReasonRefused, "has UID %q, not %q: it was recreated", pod.UID, uid)
	}
	binding := podcontract.RestoreGroupBindingOf(pod.Annotations)
	if binding.Name == "" {
		return refuse(snapshotv1alpha1.PodGroupReasonRefused, "is not a group target: it has no %s annotation",
			podcontract.RestoreGroupAnnotation)
	}
	bound := binding.Binds(restore.Name, restore.UID)
	if !bound && binding.UID != "" {
		other := &snapshotv1alpha1.PodGroupRestore{}
		switch err := reader.Get(ctx, client.ObjectKey{Namespace: restore.Namespace, Name: binding.Name}, other); {
		case err != nil && !apierrors.IsNotFound(err):
			return podGroupRestoreTargetVerdict{}, fmt.Errorf("get PodGroupRestore %q that Pod %q is bound to: %w", binding.Name, pod.Name, err)
		case err == nil && binding.Binds(other.Name, other.UID) && !podGroupRestoreFinished(&other.Status):
			return refuse(snapshotv1alpha1.PodGroupReasonTargetInUse, "is bound to PodGroupRestore %q, which has not finished", other.Name)
		}
	}
	if other, pinned := pinnedBy[pod.UID]; pinned {
		return refuse(snapshotv1alpha1.PodGroupReasonTargetInUse, "is pinned by PodGroupRestore %q, which has not finished", other)
	}
	if findPodCondition(pod, podcontract.RestoreStartedCondition) != nil {
		return refuse(snapshotv1alpha1.PodGroupReasonRefused, "carries %s: an earlier restore changed it, so restoring needs a new Pod",
			podcontract.RestoreStartedCondition)
	}
	destination := podGroupRestoreDestination(target, member)
	container := podContainerStatus(pod, destination)
	if ip := pod.Status.PodIP; ip != "" {
		if addr, err := netip.ParseAddr(ip); err != nil || !addr.Is4() {
			return refuse(snapshotv1alpha1.PodGroupReasonRefused, "has the address %q, but a group restore supports only IPv4", ip)
		}
	}
	if pod.Status.Phase == corev1.PodRunning && container == nil {
		return refuse(snapshotv1alpha1.PodGroupReasonRefused, "runs but has no container %q to restore into", destination)
	}

	switch {
	case !bound && binding.UID == "":
		return wait("is not bound to this PodGroupRestore yet: it names PodGroupRestore %q without a UID", binding.Name)
	case !bound:
		return wait("is not bound to this PodGroupRestore yet: it is bound to PodGroupRestore %q with UID %q, "+
			"an attempt that has finished or no longer exists", binding.Name, binding.UID)
	case pod.Spec.NodeName == "":
		return wait("is not scheduled to a node yet")
	case pod.Status.PodIP == "":
		return wait("has no IP address yet")
	case container == nil || container.ContainerID == "":
		return podGroupRestoreTargetVerdict{pending: fmt.Sprintf("target %q: container %q of Pod %q has no ID yet",
			target.ID, destination, pod.Name)}, nil
	}
	return podGroupRestoreTargetVerdict{pin: &snapshotv1alpha1.PodGroupRestoreMemberStatus{
		ID:          target.ID,
		Pod:         &snapshotv1alpha1.ObjectIdentity{Name: pod.Name, UID: pod.UID},
		NodeName:    pod.Spec.NodeName,
		Containers:  []snapshotv1alpha1.PodGroupRestoreContainerStatus{{Name: destination, ContainerID: container.ContainerID}},
		TargetPodIP: pod.Status.PodIP,
	}}, nil
}

// podContainerStatus returns the status of the Pod's container name, or nil.
func podContainerStatus(pod *corev1.Pod, name string) *corev1.ContainerStatus {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == name {
			return &pod.Status.ContainerStatuses[i]
		}
	}
	return nil
}

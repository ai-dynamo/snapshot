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
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// podGroupRestoreAdmission is what restore admission decided in one
// reconcile: a refusal, a wait, a pass with its pins, or the claims to send.
type podGroupRestoreAdmission struct {
	podGroupAdmission
	// pins holds, once admission passed, what to pin for each spec target, in
	// spec order.
	pins []snapshotv1alpha1.PodGroupRestoreMemberStatus
	// claims holds the claims to send, once every target is claimed or ready
	// to be. Admission then has no state: the claims make progress, and the
	// next reconcile reads the claimed Pods.
	claims []targetClaim
}

// targetClaim is one target Pod to claim: the Pod as admission read it from
// the API server, and the same Pod with the claim applied
// (podcontract.ClaimGroupTarget). The reconcile sends the difference as a
// patch conditional on original's resourceVersion.
type targetClaim struct {
	original, claimed *corev1.Pod
}

// admit runs restore admission in two stages. In each, every refusal is
// checked before any wait, so a restore that can never pass is refused at
// once rather than after its Pods or agents are ready, and a refusal before
// the claim leaves every target gated.
//
//  1. Claim, while any target is not claimed in the cache. Everything is
//     read from the API server, since the trimmed cache holds no containers
//     and a claim must start from the whole Pod. The PodGroupSnapshot and the
//     spec are checked (readRestoreTargets), then each target
//     (claimVerdict): a missing Pod waits with TargetPodPending; a Pod with
//     another UID than its podRef names, one claimed by another restore
//     (TargetInUse), one in any other state that the claim table refuses,
//     and one that podcontract.ClaimGroupTarget refuses are refused. Only
//     when every target is claimed or ready to be does admit return the
//     claims, which the reconcile sends.
//  2. Pin, once every target is claimed. It reads the cache, and confirms a
//     refusal or a pass against the API server. The claim table still
//     applies first: a target that something else changed is no longer
//     claimed, so stage 1 refuses it. Each target (pinVerdict) waits with
//     TargetPodUnscheduled while it has no node, and with TargetPodPending
//     while it has no address or destination container ID; an address that
//     is not IPv4 is refused. The agent on every target's node must then be
//     ready for groups (AgentNotReady), and admit returns the pins.
func (r *PodGroupRestoreReconciler) admit(
	ctx context.Context,
	restore *snapshotv1alpha1.PodGroupRestore,
) (podGroupRestoreAdmission, error) {
	cached, err := readRestoreTargets(ctx, r.Client, restore)
	if err != nil {
		return podGroupRestoreAdmission{}, err
	}
	if cached.allClaimed() {
		result, err := r.pinTargets(ctx, r.Client, cached.targets)
		if err != nil || result.state == podGroupAdmissionWaiting {
			return result, err
		}
	}

	live, err := readRestoreTargets(ctx, r.NonCacheReadClient, restore)
	switch {
	case err != nil:
		return podGroupRestoreAdmission{}, err
	case live.refusal != nil:
		return podGroupRestoreAdmission{podGroupAdmission: *live.refusal}, nil
	case live.allClaimed():
		return r.pinTargets(ctx, r.NonCacheReadClient, live.targets)
	}
	return claimTargets(live.targets), nil
}

// restoreTargets is what admission read of a restore through one reader:
// a refusal of its PodGroupSnapshot or spec, or its targets.
type restoreTargets struct {
	refusal *podGroupAdmission
	targets []restoreTarget
}

// allClaimed reports whether every target's Pod exists and is claimed by
// this restore.
func (t restoreTargets) allClaimed() bool {
	if t.refusal != nil {
		return false
	}
	for i := range t.targets {
		if t.targets[i].pod == nil || t.targets[i].claim != restoreTargetClaimed {
			return false
		}
	}
	return true
}

// restoreTarget is one spec target, with its Pod and that Pod's row in the
// claim table.
type restoreTarget struct {
	spec *snapshotv1alpha1.PodGroupRestoreTarget
	// want is the claim this restore makes on the Pod.
	want podcontract.RestoreGroupClaim
	// pod is the target's Pod, or nil when it does not exist.
	pod *corev1.Pod
	// claim is the Pod's row in the claim table, and why says why a Pod is
	// in use or refused.
	claim restoreTargetClaim
	why   string
}

// readRestoreTargets reads, through reader, the restore's PodGroupSnapshot
// and every target Pod, and applies the claim table to each Pod. It returns a
// refusal instead when the snapshot or the spec fails its checks
// (admitPodGroupRestoreSnapshot, podGroupRestoreSpecRefusal).
func readRestoreTargets(
	ctx context.Context,
	reader client.Reader,
	restore *snapshotv1alpha1.PodGroupRestore,
) (restoreTargets, error) {
	snapshot, refusal, err := admitPodGroupRestoreSnapshot(ctx, reader, restore)
	if err != nil || refusal != nil {
		return restoreTargets{refusal: refusal}, err
	}
	if refusal := podGroupRestoreSpecRefusal(restore, snapshot); refusal != nil {
		return restoreTargets{refusal: refusal}, nil
	}
	members := podGroupSnapshotMembersByID(snapshot)
	targets := make([]restoreTarget, len(restore.Spec.Targets))
	for i := range restore.Spec.Targets {
		spec := &restore.Spec.Targets[i]
		target := restoreTarget{spec: spec, want: podGroupRestoreClaim(restore, spec, members[spec.ID])}
		pod := &corev1.Pod{}
		switch err := reader.Get(ctx, client.ObjectKey{Namespace: restore.Namespace, Name: spec.PodRef.Name}, pod); {
		case apierrors.IsNotFound(err):
		case err != nil:
			return restoreTargets{}, fmt.Errorf("get Pod %q of target %q: %w", spec.PodRef.Name, spec.ID, err)
		default:
			target.pod = pod
			target.claim, target.why = classifyRestoreTarget(pod, target.want)
		}
		targets[i] = target
	}
	return restoreTargets{targets: targets}, nil
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

// podGroupRestoreClaim is the claim that restore makes on target's Pod: its
// UID, and the one mapping it projects onto the Pod, from member's
// checkpointed container to podGroupRestoreDestination. The spec checks have
// already matched a mapping's source to that container.
func podGroupRestoreClaim(
	restore *snapshotv1alpha1.PodGroupRestore,
	target *snapshotv1alpha1.PodGroupRestoreTarget,
	member *snapshotv1alpha1.PodGroupSnapshotMember,
) podcontract.RestoreGroupClaim {
	return podcontract.RestoreGroupClaim{UID: restore.UID, Mapping: podcontract.ContainerMapping{
		Source:      member.Source.PodRef.Containers[0],
		Destination: podGroupRestoreDestination(target, member),
	}}
}

// restoreTargetClaim is a target Pod's row in the claim table.
type restoreTargetClaim int

const (
	// restoreTargetUnclaimed is a Pod that still carries Snapshot's
	// scheduling gate and no binding: nothing has run in it.
	restoreTargetUnclaimed restoreTargetClaim = iota + 1
	// restoreTargetClaimed is a Pod that this restore claimed: ungated, and
	// bound to its UID with the mapping it projects.
	restoreTargetClaimed
	// restoreTargetInUse is a Pod bound to another restore.
	restoreTargetInUse
	// restoreTargetRefused is a Pod in any other state: it was not created
	// as a group target, or something other than Snapshot changed it.
	restoreTargetRefused
)

// classifyRestoreTarget applies the claim table to pod, for want, the claim
// this restore makes on it, and says why a Pod is in use or refused. The
// first matching row decides:
//
//	gated, unbound                                   unclaimed
//	bound to another UID                             in use
//	ungated, bound to want's UID, with want's map    claimed
//	anything else                                    refused
//
// Only the operator writes a claim, in the patch that removes the gate, and a
// gate never comes back once removed. So a binding whose claim
// podcontract.RestoreGroupClaimOf rejects, and a binding on a Pod that still
// carries the gate, were written by something else, and are refused too. The
// table never repairs a Pod.
func classifyRestoreTarget(pod *corev1.Pod, want podcontract.RestoreGroupClaim) (restoreTargetClaim, string) {
	gated := podcontract.HasRestoreGroupGate(pod)
	claim, bound, err := podcontract.RestoreGroupClaimOf(pod.Annotations)
	switch {
	case gated && !bound:
		return restoreTargetUnclaimed, ""
	case err != nil:
		return restoreTargetRefused, fmt.Sprintf("carries an invalid claim: %v", err)
	case bound && claim.UID != want.UID:
		return restoreTargetInUse, fmt.Sprintf("is claimed by another PodGroupRestore, with UID %q", claim.UID)
	case !bound:
		return restoreTargetRefused, fmt.Sprintf("is not a group target: it has neither the %s scheduling gate nor a claim",
			podcontract.RestoreGroupSchedulingGate)
	case gated:
		return restoreTargetRefused, fmt.Sprintf("is bound to this PodGroupRestore but still carries the %s scheduling gate: "+
			"something other than Snapshot wrote its binding", podcontract.RestoreGroupSchedulingGate)
	case claim.Mapping != want.Mapping:
		return restoreTargetRefused, fmt.Sprintf("is claimed by this PodGroupRestore with the container map %s, not %s",
			claim.Mapping, want.Mapping)
	}
	return restoreTargetClaimed, ""
}

// podGroupRestoreTargetVerdict is one stage's verdict on one target: at most
// one of its fields is set, and none when a claimed target has nothing to do
// in stage 1.
type podGroupRestoreTargetVerdict struct {
	// refusal refuses the restore: the target can never pass.
	refusal *podGroupAdmission
	// wait is what the target waits for, with its reason.
	wait *podGroupAdmission
	// claim is the target's claim, ready to send.
	claim *targetClaim
	// pin is what to pin for a target that is ready.
	pin *snapshotv1alpha1.PodGroupRestoreMemberStatus
}

// targetRefusal and targetWait build a verdict whose message names the target
// and its Pod.
func targetRefusal(target restoreTarget, reason, format string, args ...any) podGroupRestoreTargetVerdict {
	return podGroupRestoreTargetVerdict{refusal: &podGroupAdmission{podGroupAdmissionRefused, reason, targetMessage(target, format, args...)}}
}

func targetWait(target restoreTarget, reason, format string, args ...any) podGroupRestoreTargetVerdict {
	return podGroupRestoreTargetVerdict{wait: &podGroupAdmission{podGroupAdmissionWaiting, reason, targetMessage(target, format, args...)}}
}

func targetMessage(target restoreTarget, format string, args ...any) string {
	return fmt.Sprintf("target %q: Pod %q ", target.spec.ID, target.spec.PodRef.Name) + fmt.Sprintf(format, args...)
}

// claimTargets is stage 1 on targets read from the API server: the first
// refusal, in spec order; else a wait naming every target that waits; else
// the claims of every unclaimed target.
func claimTargets(targets []restoreTarget) podGroupRestoreAdmission {
	var waits []podGroupAdmission
	var claims []targetClaim
	for _, target := range targets {
		verdict := claimVerdict(target)
		switch {
		case verdict.refusal != nil:
			return podGroupRestoreAdmission{podGroupAdmission: *verdict.refusal}
		case verdict.wait != nil:
			waits = append(waits, *verdict.wait)
		case verdict.claim != nil:
			claims = append(claims, *verdict.claim)
		}
	}
	if wait := targetsWait(waits); wait != nil {
		return podGroupRestoreAdmission{podGroupAdmission: *wait}
	}
	return podGroupRestoreAdmission{claims: claims}
}

// claimVerdict decides stage 1 for one target. The first matching step
// decides:
//
//  1. its Pod does not exist yet: it waits, with TargetPodPending;
//  2. its Pod's UID is not the one its podRef names, if any: Refused;
//  3. the claim table says it is in use (TargetInUse) or refused (Refused);
//  4. it is claimed already: nothing to do;
//  5. podcontract.ClaimGroupTarget refuses it, typically because its
//     destination is not shaped as one: Refused;
//  6. otherwise its claim is ready to send.
//
// Nothing can have started in a target that stage 1 claims: a gated Pod has
// never run, since Kubernetes never adds a scheduling gate to an existing
// Pod, and a Pod this restore claimed is not pinned yet, while agents act only
// on pins.
func claimVerdict(target restoreTarget) podGroupRestoreTargetVerdict {
	pod := target.pod
	if pod == nil {
		return targetWait(target, snapshotv1alpha1.PodGroupReasonTargetPodPending, "does not exist yet")
	}
	if uid := target.spec.PodRef.UID; uid != "" && pod.UID != uid {
		return targetRefusal(target, snapshotv1alpha1.PodGroupReasonRefused, "has UID %q, not %q: it was recreated", pod.UID, uid)
	}
	switch target.claim {
	case restoreTargetInUse:
		return targetRefusal(target, snapshotv1alpha1.PodGroupReasonTargetInUse, "%s", target.why)
	case restoreTargetRefused:
		return targetRefusal(target, snapshotv1alpha1.PodGroupReasonRefused, "%s", target.why)
	}
	if target.claim == restoreTargetClaimed {
		return podGroupRestoreTargetVerdict{}
	}
	claimed, err := podcontract.ClaimGroupTarget(pod, target.want)
	if err != nil {
		return targetRefusal(target, snapshotv1alpha1.PodGroupReasonRefused, "cannot be claimed: %v", err)
	}
	return podGroupRestoreTargetVerdict{claim: &targetClaim{original: pod, claimed: claimed}}
}

// pinTargets is stage 2 on claimed targets read through reader: the first
// refusal, in spec order; else a wait; else AgentNotReady for the nodes'
// agents; else a pass with the pins.
func (r *PodGroupRestoreReconciler) pinTargets(
	ctx context.Context,
	reader client.Reader,
	targets []restoreTarget,
) (podGroupRestoreAdmission, error) {
	var waits []podGroupAdmission
	pins := make([]snapshotv1alpha1.PodGroupRestoreMemberStatus, len(targets))
	nodes := make(map[string]struct{}, len(targets))
	for i, target := range targets {
		verdict := pinVerdict(target)
		switch {
		case verdict.refusal != nil:
			return podGroupRestoreAdmission{podGroupAdmission: *verdict.refusal}, nil
		case verdict.wait != nil:
			waits = append(waits, *verdict.wait)
		default:
			pins[i] = *verdict.pin
			nodes[verdict.pin.NodeName] = struct{}{}
		}
	}
	if wait := targetsWait(waits); wait != nil {
		return podGroupRestoreAdmission{podGroupAdmission: *wait}, nil
	}
	wait, err := waitForPodGroupAgents(ctx, reader, r.AgentNamespace, r.AgentSelector, nodes)
	if err != nil {
		return podGroupRestoreAdmission{}, err
	}
	if wait != nil {
		return podGroupRestoreAdmission{podGroupAdmission: *wait}, nil
	}
	return podGroupRestoreAdmission{podGroupAdmission: podGroupAdmission{state: podGroupAdmissionPassed}, pins: pins}, nil
}

// pinVerdict decides stage 2 for one claimed target. Every refusal comes
// before any wait:
//
//  1. its address is not IPv4: Refused;
//  2. it is not scheduled to a node yet: it waits, with
//     TargetPodUnscheduled;
//  3. it has no address, or its destination container has no ID yet: it
//     waits, with TargetPodPending;
//  4. otherwise it is ready, with its pin.
//
// The claim checked that the destination is in the Pod's spec, and a Pod's
// containers never change, so a running Pod always reports its status; only
// its ID may be missing yet.
func pinVerdict(target restoreTarget) podGroupRestoreTargetVerdict {
	pod := target.pod
	destination := target.want.Mapping.Destination
	container := podContainerStatus(pod, destination)
	if ip := pod.Status.PodIP; ip != "" {
		if addr, err := netip.ParseAddr(ip); err != nil || !addr.Is4() {
			return targetRefusal(target, snapshotv1alpha1.PodGroupReasonRefused,
				"has the address %q, but a group restore supports only IPv4", ip)
		}
	}
	switch {
	case pod.Spec.NodeName == "":
		return targetWait(target, snapshotv1alpha1.PodGroupReasonTargetPodUnscheduled, "is not scheduled to a node yet")
	case pod.Status.PodIP == "":
		return targetWait(target, snapshotv1alpha1.PodGroupReasonTargetPodPending, "has no IP address yet")
	case container == nil || container.ContainerID == "":
		return targetWait(target, snapshotv1alpha1.PodGroupReasonTargetPodPending, "has no ID yet for container %q", destination)
	}
	return podGroupRestoreTargetVerdict{pin: &snapshotv1alpha1.PodGroupRestoreMemberStatus{
		ID:          target.spec.ID,
		Pod:         &snapshotv1alpha1.ObjectIdentity{Name: pod.Name, UID: pod.UID},
		NodeName:    pod.Spec.NodeName,
		Containers:  []snapshotv1alpha1.PodGroupRestoreContainerStatus{{Name: destination, ContainerID: container.ContainerID}},
		TargetPodIP: pod.Status.PodIP,
	}}
}

// targetsWait combines the targets' waits into one, or returns nil when none
// waits. Scheduling comes first: while any target waits with
// TargetPodUnscheduled, the wait names only those targets.
func targetsWait(waits []podGroupAdmission) *podGroupAdmission {
	if len(waits) == 0 {
		return nil
	}
	reason := snapshotv1alpha1.PodGroupReasonTargetPodPending
	for _, wait := range waits {
		if wait.reason == snapshotv1alpha1.PodGroupReasonTargetPodUnscheduled {
			reason = wait.reason
		}
	}
	var messages []string
	for _, wait := range waits {
		if wait.reason == reason {
			messages = append(messages, wait.message)
		}
	}
	return &podGroupAdmission{podGroupAdmissionWaiting, reason, strings.Join(messages, "; ")}
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

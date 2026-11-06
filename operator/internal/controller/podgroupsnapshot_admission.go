// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// admit decides whether the operator may create the group's member work,
// reading through reader. Every refusal is checked before any wait, so a
// group that can never pass is refused at once rather than after its agents
// are ready:
//
//  1. the spec checks the CRD cannot make (podGroupSnapshotSpecRefusal);
//  2. no PodSnapshot holds a member's name, except this group's own, or one an
//     earlier PodGroupSnapshot of the same name left, which admission waits
//     for garbage collection to remove;
//  3. every source Pod exists with the pinned UID;
//  4. every source Pod is scheduled, or admission waits;
//  5. the agent on every member's node is ready for groups, or admission waits.
//
// A PodSnapshot this group controls at a member's name is member work that the
// cache may not show yet. Step 2 runs before the source Pods are read, and when
// it finds one, admit returns no result at all, as if the cache had shown it:
// a group whose member work exists is never refused, and the next reconcile
// adopts it and creates the rest. The spec checks may run first, since a group
// whose immutable spec fails them was refused before any member work existed.
func (r *PodGroupSnapshotReconciler) admit(ctx context.Context, reader client.Reader, group *snapshotv1alpha1.PodGroupSnapshot) (podGroupAdmission, error) {
	if refusal := podGroupSnapshotSpecRefusal(group); refusal != nil {
		return *refusal, nil
	}

	var leftovers []string
	var collision *podGroupAdmission
	for rank := range group.Spec.Members {
		member := &group.Spec.Members[rank]
		snap := &snapshotv1alpha1.PodSnapshot{}
		name := podGroupMemberPodSnapshotName(group, member)
		switch err := reader.Get(ctx, client.ObjectKey{Namespace: group.Namespace, Name: name}, snap); {
		case apierrors.IsNotFound(err):
		case err != nil:
			return podGroupAdmission{}, fmt.Errorf("get PodSnapshot %q of member %q: %w", name, member.ID, err)
		case isMemberPodSnapshot(group, rank, snap):
			return podGroupAdmission{}, nil
		case isPreviousIncarnationPodSnapshot(group, snap):
			owner := metav1.GetControllerOf(snap)
			leftovers = append(leftovers, fmt.Sprintf("member %q: PodSnapshot %q of an earlier PodGroupSnapshot %q (uid %q) is being deleted",
				member.ID, name, owner.Name, owner.UID))
		case collision == nil:
			collision = ptr.To(podGroupRefusal(fmt.Sprintf("member %q: PodSnapshot %q already exists and does not belong to this PodGroupSnapshot",
				member.ID, name)))
		}
	}
	if collision != nil {
		return *collision, nil
	}

	var unscheduled []string
	nodes := make(map[string]struct{}, len(group.Spec.Members))
	for i := range group.Spec.Members {
		member := &group.Spec.Members[i]
		ref := member.Source.PodRef
		pod := &corev1.Pod{}
		if err := reader.Get(ctx, client.ObjectKey{Namespace: group.Namespace, Name: ref.Name}, pod); err != nil {
			if apierrors.IsNotFound(err) {
				return podGroupRefusal(fmt.Sprintf("member %q: source Pod %q not found", member.ID, ref.Name)), nil
			}
			return podGroupAdmission{}, fmt.Errorf("get source Pod %q of member %q: %w", ref.Name, member.ID, err)
		}
		switch err := validateSourcePod(ref, pod); {
		case errors.Is(err, errPodSnapshotStalePodRef):
			return podGroupRefusal(fmt.Sprintf("member %q: %v", member.ID, err)), nil
		case errors.Is(err, errPodSnapshotPodUnscheduled):
			unscheduled = append(unscheduled, fmt.Sprintf("member %q: source Pod %q is not scheduled to a node yet", member.ID, ref.Name))
		case err != nil:
			return podGroupAdmission{}, fmt.Errorf("validate source Pod of member %q: %w", member.ID, err)
		default:
			nodes[pod.Spec.NodeName] = struct{}{}
		}
	}

	if len(leftovers) > 0 {
		return podGroupAdmission{podGroupAdmissionWaiting, snapshotv1alpha1.PodGroupReasonPreviousGroupCleanup, strings.Join(leftovers, "; ")}, nil
	}
	if len(unscheduled) > 0 {
		return podGroupAdmission{podGroupAdmissionWaiting, snapshotv1alpha1.PodGroupReasonSourcePodUnscheduled, strings.Join(unscheduled, "; ")}, nil
	}
	wait, err := waitForPodGroupAgents(ctx, reader, r.AgentNamespace, r.AgentSelector, nodes)
	if err != nil {
		return podGroupAdmission{}, err
	}
	if wait != nil {
		return *wait, nil
	}
	return podGroupAdmission{state: podGroupAdmissionPassed}, nil
}

// podGroupSnapshotSpecRefusal makes the spec checks that the CRD cannot, and
// returns the first refusal, or nil:
//
//   - members name distinct source Pods: a CEL rule comparing 256 Pod names
//     exceeds the API server's cost budget;
//   - every member's template labels and annotations are valid on a
//     PodSnapshot, so no member's PodSnapshot fails to be created after its
//     peers' were.
func podGroupSnapshotSpecRefusal(group *snapshotv1alpha1.PodGroupSnapshot) *podGroupAdmission {
	members := group.Spec.Members
	sources := make(map[string]string, len(members))
	for rank, member := range members {
		name := member.Source.PodRef.Name
		if first, found := sources[name]; found {
			return ptr.To(podGroupRefusal(fmt.Sprintf("members %q and %q name the same source Pod %q", first, member.ID, name)))
		}
		sources[name] = member.ID
		if member.PodSnapshotTemplate == nil {
			continue
		}
		path := field.NewPath("spec", "members").Index(rank).Child("podSnapshotTemplate", "metadata")
		if err := validateTemplateMetadata(member.PodSnapshotTemplate.Metadata, path,
			snapshotv1alpha1.PodGroupLabel, snapshotv1alpha1.PodGroupMemberLabel); err != nil {
			return ptr.To(podGroupRefusal(fmt.Sprintf("member %q: %v", member.ID, err)))
		}
	}
	return nil
}

// isPreviousIncarnationPodSnapshot reports whether snap is controlled by an
// earlier PodGroupSnapshot with the group's name: one that was deleted, since
// its name is now this group's, and whose PodSnapshots garbage collection is
// removing.
func isPreviousIncarnationPodSnapshot(group *snapshotv1alpha1.PodGroupSnapshot, snap *snapshotv1alpha1.PodSnapshot) bool {
	owner := metav1.GetControllerOf(snap)
	return owner != nil && isPodGroupSnapshotOwner(owner) && owner.Name == group.Name && owner.UID != group.UID
}

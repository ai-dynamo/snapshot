// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package groups

import (
	"cmp"
	"time"

	"github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/ai-dynamo/snapshot/coordinator/session"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// attempt is what the coordinator reads of one group resource: a
// PodGroupSnapshot's capture or a PodGroupRestore. Its two builders,
// captureAttempt and restoreAttempt, are what read the kinds' members and
// statuses.
type attempt struct {
	// spec is the attempt's session: the resource's UID, its members with
	// their ranks, and its deadline, the resource's creation plus
	// spec.deadlineSeconds. It lists no members unless the attempt is
	// ensurable.
	spec server.Spec

	// ensurable reports whether the attempt knows its members' ranks, which
	// creating its session takes. A restore whose PodGroupSnapshot is gone
	// does not.
	ensurable bool

	// nodes maps the ID of each member to the node its Pod runs on, as the
	// resource's status records it, or to "" while it records none.
	nodes map[string]string

	// sessionNode is the node of the rank-0 member, whose coordinator serves
	// the session, or "" while the status records none or the ranks are not
	// known.
	sessionNode string

	// memberWork reports whether the operator has created work for the
	// members: a member PodSnapshot pinned, for a capture, or the members
	// pinned to their restore Pods, for a restore. Agents join the session
	// only then.
	memberWork bool

	// final reports whether the attempt is over: its Failed condition is
	// True, or every member is in a final phase.
	final bool

	// failure is the reason of the first Failed member, in rank order where
	// the ranks are known, or session.ReasonMemberFailed if that member
	// records none. It is "" while no member has failed.
	failure string

	// deleting reports whether the resource's deletion has started.
	deleting bool
}

// captureAttempt returns the attempt of a PodGroupSnapshot. A member's rank is
// its position in spec.members.
func captureAttempt(group *v1alpha1.PodGroupSnapshot) attempt {
	statuses := make(map[string]v1alpha1.PodGroupSnapshotMemberStatus, len(group.Status.Members))
	for _, m := range group.Status.Members {
		statuses[m.ID] = m
	}
	a := newAttempt(&group.ObjectMeta, group.Spec.DeadlineSeconds, group.Status.Conditions, len(group.Spec.Members))
	a.ensurable = true
	everyFinal := true
	for rank, m := range group.Spec.Members {
		status := statuses[m.ID]
		a.add(m.ID, rank, status.NodeName)
		a.memberWork = a.memberWork || status.PodSnapshot != nil
		everyFinal = everyFinal &&
			(status.Phase == v1alpha1.CheckpointPhaseCheckpointed || status.Phase == v1alpha1.CheckpointPhaseFailed)
		if status.Phase == v1alpha1.CheckpointPhaseFailed && a.failure == "" {
			a.failure = cmp.Or(status.Reason, session.ReasonMemberFailed)
		}
	}
	a.final = a.final || everyFinal
	return a
}

// restoreAttempt returns the attempt of a PodGroupRestore, whose ranks are its
// members' positions in the PodGroupSnapshot it restores, snapshot. It reports
// false when snapshot's members are not the restore's targets.
//
// When snapshot is nil, or is not the object spec.snapshotRef names, the
// attempt is read from the restore alone, in the order of its targets: it has
// the members' nodes, but not their ranks, so it is not ensurable. The in-use
// finalizer keeps a restore's PodGroupSnapshot until the restore is finished,
// so only a restore that is finished, or was refused, has none. Its released
// members may still rendezvous through its session, which is kept until it
// ends or its deadline passes.
func restoreAttempt(restore *v1alpha1.PodGroupRestore, snapshot *v1alpha1.PodGroupSnapshot) (attempt, bool) {
	ensurable := snapshot != nil && snapshot.UID == restore.Spec.SnapshotRef.UID
	var ids []string
	if ensurable {
		var ok bool
		if ids, ok = rankedTargets(restore, snapshot); !ok {
			return attempt{}, false
		}
	} else {
		for _, t := range restore.Spec.Targets {
			ids = append(ids, t.ID)
		}
	}
	statuses := make(map[string]v1alpha1.PodGroupRestoreMemberStatus, len(restore.Status.Members))
	for _, m := range restore.Status.Members {
		statuses[m.ID] = m
	}
	a := newAttempt(&restore.ObjectMeta, restore.Spec.DeadlineSeconds, restore.Status.Conditions, len(ids))
	a.ensurable = ensurable
	everyFinal := true
	for rank, id := range ids {
		status := statuses[id]
		a.add(id, rank, status.NodeName)
		a.memberWork = a.memberWork || status.Pod != nil
		everyFinal = everyFinal &&
			(status.Phase == v1alpha1.RestorePhaseReleased || status.Phase == v1alpha1.RestorePhaseFailed)
		if status.Phase == v1alpha1.RestorePhaseFailed && a.failure == "" {
			a.failure = cmp.Or(status.Reason, session.ReasonMemberFailed)
		}
	}
	a.final = a.final || everyFinal
	return a, true
}

// rankedTargets returns the IDs of a restore's targets in rank order, their
// members' order in snapshot, or false when snapshot's members are not the
// targets.
func rankedTargets(restore *v1alpha1.PodGroupRestore, snapshot *v1alpha1.PodGroupSnapshot) ([]string, bool) {
	if len(snapshot.Spec.Members) != len(restore.Spec.Targets) {
		return nil, false
	}
	targets := make(map[string]bool, len(restore.Spec.Targets))
	for _, t := range restore.Spec.Targets {
		targets[t.ID] = true
	}
	ids := make([]string, len(snapshot.Spec.Members))
	for rank, m := range snapshot.Spec.Members {
		if !targets[m.ID] {
			return nil, false
		}
		ids[rank] = m.ID
	}
	return ids, true
}

// newAttempt returns the attempt of a group resource with no members yet: its
// UID, deadline, and deletion, and whether its Failed condition makes it final.
func newAttempt(obj *metav1.ObjectMeta, deadlineSeconds int64, conditions []metav1.Condition, members int) attempt {
	return attempt{
		spec: server.Spec{
			UID:      obj.UID,
			Members:  make([]session.Member, 0, members),
			Deadline: obj.CreationTimestamp.Add(time.Duration(deadlineSeconds) * time.Second),
		},
		nodes:    make(map[string]string, members),
		final:    meta.IsStatusConditionTrue(conditions, v1alpha1.PodGroupConditionFailed),
		deleting: obj.DeletionTimestamp != nil,
	}
}

// add adds the member id at rank, running on node. Of an attempt that is not
// ensurable, whose ranks are not known, it records only the node.
func (a *attempt) add(id string, rank int, node string) {
	a.nodes[id] = node
	if !a.ensurable {
		return
	}
	a.spec.Members = append(a.spec.Members, session.Member{ID: id, Rank: rank})
	if rank == 0 {
		a.sessionNode = node
	}
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package groups

import (
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// created is when the test's group resources were created.
var created = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

// newSnapshot returns a PodGroupSnapshot of the members ids, in rank order,
// with a deadline 10 minutes after its creation and no status.
func newSnapshot(uid types.UID, ids ...string) *v1alpha1.PodGroupSnapshot {
	g := &v1alpha1.PodGroupSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "group", UID: uid,
			CreationTimestamp: metav1.NewTime(created)},
		Spec: v1alpha1.PodGroupSnapshotSpec{DeadlineSeconds: 600},
	}
	for _, id := range ids {
		g.Spec.Members = append(g.Spec.Members, v1alpha1.PodGroupSnapshotMember{ID: id})
	}
	return g
}

// newRestore returns a PodGroupRestore of snapshot's members, with the targets
// ids, in that order, a deadline 5 minutes after its creation, and no status.
func newRestore(snapshot *v1alpha1.PodGroupSnapshot, ids ...string) *v1alpha1.PodGroupRestore {
	r := &v1alpha1.PodGroupRestore{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "restore", UID: "restore-uid",
			CreationTimestamp: metav1.NewTime(created)},
		Spec: v1alpha1.PodGroupRestoreSpec{
			SnapshotRef:     v1alpha1.ObjectIdentity{Name: snapshot.Name, UID: snapshot.UID},
			DeadlineSeconds: 300,
		},
	}
	for _, id := range ids {
		r.Spec.Targets = append(r.Spec.Targets, v1alpha1.PodGroupRestoreTarget{ID: id,
			PodRef: v1alpha1.TargetPodReference{Name: id}})
	}
	return r
}

// failed is a Failed condition that is True.
var failed = []metav1.Condition{{Type: v1alpha1.PodGroupConditionFailed, Status: metav1.ConditionTrue,
	Reason: v1alpha1.PodGroupReasonMemberFailed}}

func TestCaptureAttempt(t *testing.T) {
	now := metav1.NewTime(created.Add(time.Minute))

	t.Run("ranks, nodes and deadline", func(t *testing.T) {
		g := newSnapshot("snapshot-uid", "b", "a", "c")
		g.Status.Members = []v1alpha1.PodGroupSnapshotMemberStatus{
			{ID: "a", Rank: 1, NodeName: "node-a"},
			{ID: "b", Rank: 0, NodeName: "node-b"},
		}
		a := captureAttempt(g)
		assert.Equal(t, types.UID("snapshot-uid"), a.spec.UID)
		assert.Equal(t, []session.Member{{ID: "b", Rank: 0}, {ID: "a", Rank: 1}, {ID: "c", Rank: 2}}, a.spec.Members,
			"a capture's ranks are its spec order")
		assert.Equal(t, created.Add(10*time.Minute), a.spec.Deadline)
		assert.Equal(t, map[string]string{"a": "node-a", "b": "node-b", "c": ""}, a.nodes,
			"a member with no status yet has no node")
		assert.Equal(t, "node-b", a.sessionNode)
		assert.False(t, a.memberWork)
		assert.False(t, a.final)
		assert.False(t, a.deleting)
	})

	t.Run("the rank-0 member's node is not known yet", func(t *testing.T) {
		g := newSnapshot("snapshot-uid", "a", "b")
		g.Status.Members = []v1alpha1.PodGroupSnapshotMemberStatus{{ID: "b", Rank: 1, NodeName: "node-b"}}
		assert.Empty(t, captureAttempt(g).sessionNode)
	})

	t.Run("member work exists once a member PodSnapshot is pinned", func(t *testing.T) {
		g := newSnapshot("snapshot-uid", "a", "b")
		g.Status.Members = []v1alpha1.PodGroupSnapshotMemberStatus{
			{ID: "a", NodeName: "node-a"},
			{ID: "b", Rank: 1, NodeName: "node-b", PodSnapshot: &v1alpha1.ObjectIdentity{Name: "group-b", UID: "ps"}},
		}
		assert.True(t, captureAttempt(g).memberWork)
	})

	for _, tt := range []struct {
		name       string
		phases     []v1alpha1.CheckpointPhase
		conditions []metav1.Condition
		final      bool
	}{
		{"no member final", []v1alpha1.CheckpointPhase{v1alpha1.CheckpointPhaseCheckpointing,
			v1alpha1.CheckpointPhaseAdmitted}, nil, false},
		{"one member final", []v1alpha1.CheckpointPhase{v1alpha1.CheckpointPhaseCheckpointed,
			v1alpha1.CheckpointPhaseCheckpointing}, nil, false},
		{"every member final", []v1alpha1.CheckpointPhase{v1alpha1.CheckpointPhaseCheckpointed,
			v1alpha1.CheckpointPhaseFailed}, nil, true},
		{"a member with no status", []v1alpha1.CheckpointPhase{v1alpha1.CheckpointPhaseCheckpointed}, nil, false},
		{"Failed", []v1alpha1.CheckpointPhase{v1alpha1.CheckpointPhaseCheckpointing,
			v1alpha1.CheckpointPhaseAdmitted}, failed, true},
	} {
		t.Run("final: "+tt.name, func(t *testing.T) {
			g := newSnapshot("snapshot-uid", "a", "b")
			for i, phase := range tt.phases {
				g.Status.Members = append(g.Status.Members, v1alpha1.PodGroupSnapshotMemberStatus{
					ID: g.Spec.Members[i].ID, Rank: int32(i), Phase: phase})
			}
			g.Status.Conditions = tt.conditions
			assert.Equal(t, tt.final, captureAttempt(g).final)
		})
	}

	t.Run("deleting", func(t *testing.T) {
		g := newSnapshot("snapshot-uid", "a")
		g.DeletionTimestamp = &now
		assert.True(t, captureAttempt(g).deleting)
	})
}

func TestRestoreAttempt(t *testing.T) {
	now := metav1.NewTime(created.Add(time.Minute))
	snapshot := newSnapshot("snapshot-uid", "b", "a", "c")

	t.Run("ranks come from the PodGroupSnapshot", func(t *testing.T) {
		r := newRestore(snapshot, "a", "c", "b")
		r.Status.Members = []v1alpha1.PodGroupRestoreMemberStatus{
			{ID: "a", NodeName: "node-a"},
			{ID: "b", NodeName: "node-b"},
		}
		a, ok := restoreAttempt(r, snapshot)
		require.True(t, ok)
		assert.Equal(t, types.UID("restore-uid"), a.spec.UID)
		assert.Equal(t, []session.Member{{ID: "b", Rank: 0}, {ID: "a", Rank: 1}, {ID: "c", Rank: 2}},
			a.spec.Members)
		assert.Equal(t, created.Add(5*time.Minute), a.spec.Deadline)
		assert.Equal(t, map[string]string{"a": "node-a", "b": "node-b", "c": ""}, a.nodes,
			"a member not pinned yet has no node")
		assert.Equal(t, "node-b", a.sessionNode)
		assert.False(t, a.memberWork)
		assert.False(t, a.final)
		assert.False(t, a.deleting)
	})

	t.Run("no attempt", func(t *testing.T) {
		recreated := newSnapshot("recreated-uid", "b", "a", "c")
		for name, tt := range map[string]struct {
			restore  *v1alpha1.PodGroupRestore
			snapshot *v1alpha1.PodGroupSnapshot
		}{
			"the PodGroupSnapshot is missing":    {newRestore(snapshot, "a", "b", "c"), nil},
			"the PodGroupSnapshot was recreated": {newRestore(snapshot, "a", "b", "c"), recreated},
			"a member has no target":             {newRestore(snapshot, "a", "b"), snapshot},
			"a target is no member":              {newRestore(snapshot, "a", "b", "d"), snapshot},
			"a target too many":                  {newRestore(snapshot, "a", "b", "c", "d"), snapshot},
		} {
			t.Run(name, func(t *testing.T) {
				_, ok := restoreAttempt(tt.restore, tt.snapshot)
				assert.False(t, ok)
			})
		}
	})

	t.Run("member work exists once the members are pinned", func(t *testing.T) {
		r := newRestore(snapshot, "a", "b", "c")
		for _, id := range []string{"a", "b", "c"} {
			r.Status.Members = append(r.Status.Members, v1alpha1.PodGroupRestoreMemberStatus{ID: id,
				NodeName: "node-" + id, Pod: &v1alpha1.ObjectIdentity{Name: id, UID: types.UID("pod-" + id)}})
		}
		a, ok := restoreAttempt(r, snapshot)
		require.True(t, ok)
		assert.True(t, a.memberWork)
	})

	for _, tt := range []struct {
		name       string
		phases     []v1alpha1.RestorePhase
		conditions []metav1.Condition
		final      bool
	}{
		{"no member final", []v1alpha1.RestorePhase{v1alpha1.RestorePhaseRestoring, v1alpha1.RestorePhaseRestoring,
			v1alpha1.RestorePhaseAdmitted}, nil, false},
		{"one member final", []v1alpha1.RestorePhase{v1alpha1.RestorePhaseReleased, v1alpha1.RestorePhaseRestoring,
			v1alpha1.RestorePhaseRestoring}, nil, false},
		{"every member final", []v1alpha1.RestorePhase{v1alpha1.RestorePhaseReleased, v1alpha1.RestorePhaseFailed,
			v1alpha1.RestorePhaseReleased}, nil, true},
		{"a member with no status", []v1alpha1.RestorePhase{v1alpha1.RestorePhaseReleased,
			v1alpha1.RestorePhaseReleased}, nil, false},
		{"Failed", []v1alpha1.RestorePhase{v1alpha1.RestorePhaseRestoring, v1alpha1.RestorePhaseRestoring,
			v1alpha1.RestorePhaseRestoring}, failed, true},
	} {
		t.Run("final: "+tt.name, func(t *testing.T) {
			r := newRestore(snapshot, "a", "b", "c")
			for i, phase := range tt.phases {
				r.Status.Members = append(r.Status.Members, v1alpha1.PodGroupRestoreMemberStatus{
					ID: r.Spec.Targets[i].ID, Phase: phase})
			}
			r.Status.Conditions = tt.conditions
			a, ok := restoreAttempt(r, snapshot)
			require.True(t, ok)
			assert.Equal(t, tt.final, a.final)
		})
	}

	t.Run("deleting", func(t *testing.T) {
		r := newRestore(snapshot, "a", "b", "c")
		r.DeletionTimestamp = &now
		a, ok := restoreAttempt(r, snapshot)
		require.True(t, ok)
		assert.True(t, a.deleting)
	})
}

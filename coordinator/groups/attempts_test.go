// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package groups

import (
	"context"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeReader returns a client that reads objs, indexed as Setup indexes them.
func fakeReader(t *testing.T, objs ...client.Object) *fake.ClientBuilder {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithIndex(&v1alpha1.PodGroupSnapshot{}, uidField, indexUID).
		WithIndex(&v1alpha1.PodGroupRestore{}, uidField, indexUID).
		WithIndex(&v1alpha1.PodGroupRestore{}, snapshotRefField, indexSnapshotRef)
}

// fakeAttempts returns Attempts that read objs, indexed as Setup indexes them.
func fakeAttempts(t *testing.T, objs ...client.Object) *Attempts {
	t.Helper()
	return &Attempts{reader: fakeReader(t, objs...).Build()}
}

// assertAttempt asserts that got is want. A client reads a creation time back
// in the local time zone, so the deadlines are compared as instants.
func assertAttempt(t *testing.T, want attempt, got *attempt) {
	t.Helper()
	require.NotNil(t, got)
	assert.True(t, want.spec.Deadline.Equal(got.spec.Deadline), "deadline %v, want %v", got.spec.Deadline,
		want.spec.Deadline)
	want.spec.Deadline = time.Time{}
	unzoned := *got
	unzoned.spec.Deadline = time.Time{}
	assert.Equal(t, want, unzoned)
}

func TestAttempts(t *testing.T) {
	ctx := context.Background()
	snapshot := newSnapshot("snapshot-uid", "a", "b")
	snapshot.Status.Members = []v1alpha1.PodGroupSnapshotMemberStatus{{ID: "a", NodeName: "node-a"}}
	restore := newRestore(snapshot, "b", "a")
	restore.Status.Members = []v1alpha1.PodGroupRestoreMemberStatus{{ID: "b", NodeName: "node-b"}}
	as := fakeAttempts(t, snapshot, restore)

	t.Run("get", func(t *testing.T) {
		a, err := as.get(ctx, snapshot.UID)
		require.NoError(t, err)
		assertAttempt(t, captureAttempt(snapshot), a)
		a, err = as.get(ctx, restore.UID)
		require.NoError(t, err)
		want, ok := restoreAttempt(restore, snapshot)
		require.True(t, ok)
		assertAttempt(t, want, a)
		a, err = as.get(ctx, "unknown")
		require.ErrorIs(t, err, session.ErrNotFound)
		assert.Nil(t, a)
	})

	t.Run("MemberNodes", func(t *testing.T) {
		nodes, err := as.MemberNodes(ctx, snapshot.UID)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"a": "node-a", "b": ""}, nodes)
		nodes, err = as.MemberNodes(ctx, restore.UID)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"a": "", "b": "node-b"}, nodes)
		_, err = as.MemberNodes(ctx, "unknown")
		require.ErrorIs(t, err, session.ErrNotFound)
	})

	t.Run("a restore whose PodGroupSnapshot is gone has no attempt", func(t *testing.T) {
		as := fakeAttempts(t, restore)
		a, err := as.get(ctx, restore.UID)
		require.ErrorIs(t, err, session.ErrNotFound)
		assert.Nil(t, a)
		_, err = as.MemberNodes(ctx, restore.UID)
		require.ErrorIs(t, err, session.ErrNotFound)
	})
}

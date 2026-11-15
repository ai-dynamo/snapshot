// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateAttempt covers the longest attempt UID.
func TestValidateAttempt(t *testing.T) {
	require.NoError(t, ValidateAttempt(strings.Repeat("u", MaxAttemptLength)))
	require.ErrorIs(t, ValidateAttempt(strings.Repeat("u", MaxAttemptLength+1)), ErrInvalid)
	require.Len(t, "KV_"+SnapshotBucket(strings.Repeat("u", MaxAttemptLength)), maxStreamName)
}

// TestUserName round-trips a member's user name, and refuses any other.
func TestUserName(t *testing.T) {
	attempt, member, ok := ParseUserName(UserName("0b7e-uid", "worker-1"))
	require.True(t, ok)
	assert.Equal(t, "0b7e-uid", attempt)
	assert.Equal(t, "worker-1", member)
	for _, user := range []string{"", "admin", "uid/", "/member", "uid/m/x", "uid.x/m"} {
		_, _, ok := ParseUserName(user)
		assert.False(t, ok, "user %q", user)
	}
}

// TestValidateName covers what may name an attempt, a member, or a barrier.
func TestValidateName(t *testing.T) {
	for _, name := range []string{"a", "admitted", "net-freeze", "Pod_0", "4c1f1a5e-9b4b-4f0c-9a51-3a7d1c2e9f00",
		strings.Repeat("n", MaxNameLength)} {
		require.NoError(t, ValidateName(name), "name %q", name)
	}
	for _, name := range []string{"", "a.b", "a/b", "*", ">", "a b", "é", strings.Repeat("n", MaxNameLength+1)} {
		require.ErrorIs(t, ValidateName(name), ErrInvalid, "name %q", name)
	}
}

// TestNamespaceBucket checks the one mapping from a namespace to its bucket,
// and that every bucket name fits a stream name.
func TestNamespaceBucket(t *testing.T) {
	assert.Equal(t, SnapshotBucket("a"), NamespaceSnapshot.Bucket("a"))
	assert.Equal(t, SnapshotBucket("a"), NamespaceCuInterpose.Bucket("a"))
	assert.Equal(t, LibraryBucket("a"), NamespaceLibrary.Bucket("a"))
	assert.ElementsMatch(t, []string{ControlBucket("a"), SnapshotBucket("a"), LibraryBucket("a")}, Buckets("a"))
	for _, bucket := range Buckets(strings.Repeat("u", MaxAttemptLength)) {
		assert.LessOrEqual(t, len("KV_"+bucket), maxStreamName, bucket)
	}
}

// TestMemberPermissionsFollowTheBuckets checks that a member may write each
// namespace in the bucket that holds it, and each control key in the control
// bucket, and nowhere else.
func TestMemberPermissionsFollowTheBuckets(t *testing.T) {
	publish := MemberPermissions("a", "m").Publish
	for _, subject := range []string{
		"$KV." + NamespaceSnapshot.Bucket("a") + ".snapshot.>",
		"$KV." + NamespaceCuInterpose.Bucket("a") + ".cuinterpose.>",
		"$KV." + NamespaceLibrary.Bucket("a") + ".>",
		"$KV." + ControlBucket("a") + ".ended",
		"$KV." + ControlBucket("a") + ".barrier.*.m",
		"$KV." + ControlBucket("a") + ".left.m",
	} {
		assert.Contains(t, publish, subject)
	}
	for _, subject := range publish {
		assert.NotContains(t, subject, ControlBucket("a")+".snapshot", "an agent's key must not reach the control bucket")
		assert.NotContains(t, subject, SnapshotBucket("a")+".ended", "a control key must not reach the Snapshot bucket")
		assert.NotContains(t, subject, ".meta", "no member writes meta")
	}
}

// TestControlBytes checks that the control bucket's size grows with the
// membership.
func TestControlBytes(t *testing.T) {
	assert.Equal(t, int64(ControlBytesBase+ControlBytesPerMember), ControlBytes(1))
	assert.Equal(t, int64(ControlBytesBase+256*ControlBytesPerMember), ControlBytes(256))
}

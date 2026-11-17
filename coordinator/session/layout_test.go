// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

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
// which refuses any other namespace, and that every bucket name fits a stream
// name.
func TestNamespaceBucket(t *testing.T) {
	for ns, want := range map[Namespace]string{
		NamespaceSnapshot:    SnapshotBucket("a"),
		NamespaceCuInterpose: SnapshotBucket("a"),
		NamespaceLibrary:     LibraryBucket("a"),
	} {
		bucket, err := ns.Bucket("a")
		require.NoError(t, err)
		assert.Equal(t, want, bucket, "namespace %s", ns)
	}
	assert.ElementsMatch(t, []Namespace{NamespaceSnapshot, NamespaceCuInterpose, NamespaceLibrary}, Namespaces())
	for _, ns := range []Namespace{"", "other", "Snapshot"} {
		_, err := ns.Bucket("a")
		require.ErrorIs(t, err, ErrInvalid, "namespace %q", ns)
	}
	assert.ElementsMatch(t, []string{ControlBucket("a"), SnapshotBucket("a"), LibraryBucket("a")}, Buckets("a"))
	for _, bucket := range Buckets(strings.Repeat("u", MaxAttemptLength)) {
		assert.LessOrEqual(t, len("KV_"+bucket), maxStreamName, bucket)
	}
}

// TestMemberPermissionsFollowTheBuckets checks that a member may write each
// namespace in the bucket that holds it, and each control key in the control
// bucket, and nowhere else.
func TestMemberPermissionsFollowTheBuckets(t *testing.T) {
	permissions := MemberPermissions("a", "m")
	publish := permissions.Publish
	for _, ns := range Namespaces() {
		bucket, err := ns.Bucket("a")
		require.NoError(t, err)
		assert.Contains(t, publish, "$KV."+bucket+"."+string(ns)+".>")
	}
	for _, subject := range []string{
		"$KV." + ControlBucket("a") + ".ended",
		"$KV." + ControlBucket("a") + ".barrier.*.m",
		"$KV." + ControlBucket("a") + ".left.m",
	} {
		assert.Contains(t, publish, subject)
	}
	for _, subject := range publish {
		assert.NotContains(t, subject, ControlBucket("a")+".snapshot", "an agent's key must not reach the control bucket")
		assert.NotContains(t, subject, SnapshotBucket("a")+".ended", "a control key must not reach the Snapshot bucket")
		assert.NotContains(t, subject, SnapshotBucket("a")+".library", "a library key must not reach the Snapshot bucket")
		assert.NotContains(t, subject, LibraryBucket("a")+".snapshot", "an agent's key must not reach the library bucket")
		assert.NotContains(t, subject, ".meta", "no member writes meta")
	}
	assert.Len(t, publish, cap(publish), "the subjects must fill what was allocated for them")
	assert.Equal(t, []string{"_INBOX.a.m.>"}, permissions.Subscribe)
}

// TestReadOnlyPermissionsWriteNoKey checks that a read-only member may make
// every request a member may, and write no key.
func TestReadOnlyPermissionsWriteNoKey(t *testing.T) {
	member, readOnly := MemberPermissions("a", "m"), ReadOnlyPermissions("a", "m")
	requests := slices.DeleteFunc(slices.Clone(member.Publish), func(subject string) bool {
		return strings.HasPrefix(subject, "$KV.")
	})
	assert.Equal(t, requests, readOnly.Publish)
	assert.Len(t, readOnly.Publish, cap(readOnly.Publish), "the subjects must fill what was allocated for them")
	assert.Equal(t, member.Subscribe, readOnly.Subscribe)
}

// TestControlBytes checks the control bucket's size against its parts, with
// the longest names: each member adds its arrivals and its departure, each as
// large as the largest record, an arrival of the longest names with the
// longest reason's bytes, and the session adds its end and the largest Meta.
func TestControlBytes(t *testing.T) {
	attempt, name := strings.Repeat("u", MaxAttemptLength), strings.Repeat("n", MaxNameLength)
	arrival := BarrierKey(name, name)
	for _, key := range []string{KeyMeta, KeyEnded, LeftKey(name)} {
		assert.Less(t, len(key), len(arrival), "an arrival must have the longest key")
	}
	record := recordFraming + len("$KV."+ControlBucket(attempt)+"."+arrival) + MaxControlValue
	require.Equal(t, record, maxControlRecord())
	for _, members := range []int{1, 2, MaxMembers} {
		meta := recordFraming + len("$KV."+ControlBucket(attempt)+".meta") + MaxMetaBytes(members)
		assert.Equal(t, int64((members*(ArrivalsPerMember+1)+1)*record+meta), ControlBytes(members),
			"%d members", members)
	}
}

// TestMaxControlValue checks that the longest reason fills the largest value
// of a control key, and that no longer one is a reason.
func TestMaxControlValue(t *testing.T) {
	require.NoError(t, ValidateReason(strings.Repeat("A", MaxControlValue)))
	require.ErrorIs(t, ValidateReason(strings.Repeat("A", MaxControlValue+1)), ErrInvalid)
}

// TestMaxMetaBytes checks that the largest Meta of a membership holds every
// Meta of it, with each field at its widest, and only those.
func TestMaxMetaBytes(t *testing.T) {
	for _, members := range []int{1, 10, MaxMembers} {
		meta := Meta{
			Deadline:        time.Date(2026, 10, 30, 12, 0, 0, 123456789, time.FixedZone("", 5*60*60+30*60)),
			Epoch:           math.MaxUint64,
			MaxValue:        math.MaxInt,
			MaxLibraryValue: math.MaxInt,
		}
		for rank := range members {
			meta.Members = append(meta.Members, Member{ID: strings.Repeat("m", MaxNameLength), Rank: rank})
		}
		encoded, err := json.Marshal(meta)
		require.NoError(t, err)
		assert.LessOrEqual(t, len(encoded), MaxMetaBytes(members), "%d members", members)
		// Every field is at its widest but the ranks below the last, which
		// take fewer digits.
		slack := 0
		for rank := range members - 1 {
			slack += len(strconv.Itoa(members-1)) - len(strconv.Itoa(rank))
		}
		assert.Equal(t, len(encoded)+slack, MaxMetaBytes(members), "%d members", members)
	}
}

// TestMaxPending checks that what the coordinator buffers for a connection
// holds the replies of session.MaxConcurrentGets reads of the Snapshot
// bucket's largest values, and of a library read beside them.
func TestMaxPending(t *testing.T) {
	assert.LessOrEqual(t, ReplyHeadroom, PayloadHeadroom)
	replies := int64(MaxConcurrentGets*(MaxValue+ReplyHeadroom) + MaxLibraryValue + ReplyHeadroom)
	assert.LessOrEqual(t, replies, MaxPending(MaxValue))
}

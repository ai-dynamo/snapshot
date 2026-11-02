// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMemberPermissions pins the subjects a member's connection may use. The
// coordinator's tests check what they allow and refuse; this one keeps a change
// to them deliberate.
func TestMemberPermissions(t *testing.T) {
	p := MemberPermissions("uid", "m0")
	assert.Equal(t, []string{
		"$JS.API.STREAM.INFO.KV_snapshot-v1-uid-s",
		"$JS.API.DIRECT.GET.KV_snapshot-v1-uid-s.>",
		"$JS.API.CONSUMER.CREATE.KV_snapshot-v1-uid-s.>",
		"$JS.API.CONSUMER.DELETE.KV_snapshot-v1-uid-s.*",
		"$JS.FC.KV_snapshot-v1-uid-s.>",
		"$JS.API.STREAM.INFO.KV_snapshot-v1-uid-l",
		"$JS.API.DIRECT.GET.KV_snapshot-v1-uid-l.>",
		"$JS.API.CONSUMER.CREATE.KV_snapshot-v1-uid-l.>",
		"$JS.API.CONSUMER.DELETE.KV_snapshot-v1-uid-l.*",
		"$JS.FC.KV_snapshot-v1-uid-l.>",
		"$KV.snapshot-v1-uid-s.snapshot.>",
		"$KV.snapshot-v1-uid-s.cuinterpose.>",
		"$KV.snapshot-v1-uid-s.ended",
		"$KV.snapshot-v1-uid-s.barrier.*.m0",
		"$KV.snapshot-v1-uid-s.left.m0",
		"$KV.snapshot-v1-uid-l.>",
	}, p.Publish)
	assert.Equal(t, []string{"_INBOX.uid.m0.>"}, p.Subscribe)
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

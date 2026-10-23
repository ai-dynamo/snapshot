// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestARoleReachesOnlyItsOwnNamespace(t *testing.T) {
	cudart := Caller{Member: memberA, Role: RoleLibrary, Library: "cudart", Version: "13.0"}
	nccl := ncclOf(memberA)
	versioned := Caller{Member: memberA, Role: RoleLibrary, Library: LibraryNCCL, Version: "2.28"}
	tests := []struct {
		name   string
		caller Caller
		key    string
		want   error
	}{
		{name: "an agent in its own namespace", caller: agentOf(memberA), key: agentKey},
		{name: "an agent in the relays' namespace", caller: agentOf(memberA), key: relayKey, want: ErrForbidden},
		{name: "an agent in a library's namespace", caller: agentOf(memberA), key: libraryKey, want: ErrForbidden},
		{name: "an agent on the bare prefix", caller: agentOf(memberA), key: agentNamespace, want: ErrForbidden},
		{name: "a relay in its own namespace", caller: relayOf(memberA), key: relayKey},
		{name: "a relay in the agents' namespace", caller: relayOf(memberA), key: agentKey, want: ErrForbidden},
		{name: "a library in its own version", caller: cudart, key: "cuda/cudart/13.0/handle"},
		{name: "a library in another version", caller: cudart, key: "cuda/cudart/12.0/handle", want: ErrForbidden},
		{name: "a library in another library", caller: cudart, key: "cuda/cublas/13.0/handle", want: ErrForbidden},
		{name: "a library in the agents' namespace", caller: cudart, key: agentKey, want: ErrForbidden},
		{name: "the NCCL endpoint in its own namespace", caller: nccl, key: ncclKey},
		{name: "the NCCL endpoint under cuda", caller: nccl, key: libraryKey, want: ErrForbidden},
		{name: "NCCL with a version under cuda", caller: versioned, key: libraryKey},
		{name: "NCCL with a version in the flat namespace", caller: versioned, key: ncclKey, want: ErrForbidden},
		{
			name:   "another library without a version",
			caller: Caller{Member: memberA, Role: RoleLibrary, Library: "cudart"},
			key:    ncclKey,
			want:   ErrForbidden,
		},
		{
			name:   "a library with no library named",
			caller: Caller{Member: memberA, Role: RoleLibrary},
			key:    "cuda//handle",
			want:   ErrForbidden,
		},
		{
			name:   "a library whose version crosses a namespace",
			caller: Caller{Member: memberA, Role: RoleLibrary, Library: "cudart", Version: "13.0/../.."},
			key:    "cuda/cudart/13.0/handle",
			want:   ErrForbidden,
		},
		{name: "a caller with no role", caller: Caller{Member: memberA}, key: agentKey, want: ErrForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, memberA)
			ctx := context.Background()

			err := f.session.Insert(ctx, Request{Caller: tt.caller}, tt.key, []byte("value"))

			if tt.want != nil {
				require.ErrorIs(t, err, tt.want)
				return
			}
			require.NoError(t, err)
			value, err := f.session.Get(ctx, tt.caller, tt.key, 0)
			require.NoError(t, err)
			assert.Equal(t, []byte("value"), value)
		})
	}
}

func TestANamespaceIsEnforcedOnBarriersToo(t *testing.T) {
	f := newFixture(t, memberA)
	ctx := context.Background()
	library := Request{Caller: ncclOf(memberA)}

	require.ErrorIs(t, f.session.CreateBarrier(ctx, library, barrierName, 1), ErrForbidden)
	require.ErrorIs(t, f.session.PostBarrier(ctx, library, barrierName, "p-1"), ErrForbidden)
	require.ErrorIs(t, f.session.AwaitBarrier(ctx, library.Caller, barrierName), ErrForbidden)
}

func TestAValueLimitFollowsTheCallersRole(t *testing.T) {
	// One buffer serves every case: 32 MiB plus the byte that goes over it.
	buffer := make([]byte, MaxValue+1)
	tests := []struct {
		name   string
		caller Caller
		key    string
		size   int
		want   error
	}{
		{name: "an agent below its limit", caller: agentOf(memberA), key: agentKey, size: MaxValue - 1},
		{name: "an agent at its limit", caller: agentOf(memberA), key: agentKey, size: MaxValue},
		{name: "an agent over its limit", caller: agentOf(memberA), key: agentKey, size: MaxValue + 1, want: ErrTooLarge},
		{name: "a library below its limit", caller: ncclOf(memberA), key: ncclKey, size: MaxLibraryValue - 1},
		{name: "a library at its limit", caller: ncclOf(memberA), key: ncclKey, size: MaxLibraryValue},
		{
			name:   "a library over its limit",
			caller: ncclOf(memberA),
			key:    ncclKey,
			size:   MaxLibraryValue + 1,
			want:   ErrTooLarge,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, memberA)

			err := f.session.Insert(context.Background(), Request{Caller: tt.caller}, tt.key, buffer[:tt.size])

			if tt.want != nil {
				require.ErrorIs(t, err, tt.want)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestANameIsLimitedInLength(t *testing.T) {
	tests := []struct {
		name   string
		length int
		want   error
	}{
		{name: "below the limit", length: MaxKeyLength - 1},
		{name: "at the limit", length: MaxKeyLength},
		{name: "over the limit", length: MaxKeyLength + 1, want: ErrTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, memberA)
			ctx := context.Background()
			key := agentNamespace + strings.Repeat("k", tt.length-len(agentNamespace))
			library := Request{Caller: ncclOf(memberA)}
			participant := ParticipantID(strings.Repeat("p", tt.length))

			errs := []error{
				f.session.Insert(ctx, requestOf(memberA, ""), key, []byte("value")),
				f.session.CreateBarrier(ctx, requestOf(memberA, ""), key, 1),
				f.session.PostBarrier(ctx, library, ncclOther, participant),
			}

			for _, err := range errs {
				if tt.want != nil {
					require.ErrorIs(t, err, tt.want)
					continue
				}
				require.NoError(t, err)
			}
		})
	}
}

func TestACompareAndSwapChecksBothValuesAgainstTheLimit(t *testing.T) {
	f := newFixture(t, memberA)
	ctx := context.Background()
	library := Request{Caller: ncclOf(memberA)}
	require.NoError(t, f.session.Insert(ctx, library, ncclKey, []byte("old")))

	err := f.session.CompareAndSwap(ctx, library, ncclKey, []byte("old"), make([]byte, MaxLibraryValue+1))

	require.ErrorIs(t, err, ErrTooLarge)
}

func TestWhoIsNotAMemberReachesNothing(t *testing.T) {
	f := newFixture(t, memberA)
	ctx, stranger := context.Background(), agentOf("member-z")

	require.ErrorIs(t, f.session.Insert(ctx, Request{Caller: stranger}, agentKey, []byte("value")), ErrNotMember)
	_, err := f.session.Get(ctx, stranger, agentKey, 0)
	require.ErrorIs(t, err, ErrNotMember)
	require.ErrorIs(t, f.session.CreateBarrier(ctx, Request{Caller: stranger}, barrierName, 1), ErrNotMember)
	require.ErrorIs(t, f.session.AwaitBarrier(ctx, stranger, barrierName), ErrNotMember)
}

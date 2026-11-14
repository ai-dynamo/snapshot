// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package group

import (
	"context"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/types"

	"github.com/ai-dynamo/snapshot/coordinator/coordinatortest"
	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/ai-dynamo/snapshot/coordinator/session"
)

func TestAdmissionGathersEveryMember(t *testing.T) {
	members := joinAll(t, 3)
	captured := []netip.Addr{
		netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("10.0.0.3"),
	}
	now := []netip.Addr{
		netip.MustParseAddr("10.1.0.1"), netip.MustParseAddr("10.1.0.2"), netip.MustParseAddr("10.1.0.3"),
	}
	each(t, members, func(ctx context.Context, m *Member) error {
		return m.Admit(ctx, Admission{PodIP: now[m.Rank()], CaptureID: "capture-1"})
	})

	for _, m := range members {
		require.Len(t, m.Admissions(), 3)
		for rank, a := range m.Admissions() {
			require.Equal(t, now[rank], a.PodIP)
		}
	}
	peers, err := members[1].PeerIPs(captured)
	require.NoError(t, err)
	require.Equal(t, map[netip.Addr]netip.Addr{captured[0]: now[0], captured[2]: now[2]}, peers)
	_, err = members[1].PeerIPs(captured[:2])
	require.ErrorContains(t, err, "the capture had 2 members, the restore has 3")

	each(t, members, func(ctx context.Context, m *Member) error { return m.Leave(ctx) })
}

func TestAdmissionRefusesMembersRestoringDifferentCaptures(t *testing.T) {
	members := joinAll(t, 2)
	errs := make([]error, len(members))
	var g errgroup.Group
	for rank, m := range members {
		g.Go(func() error {
			errs[rank] = m.Admit(within(t), Admission{PodIP: netip.MustParseAddr("10.1.0.1"), CaptureID: m.ID()})
			return nil
		})
	}
	require.NoError(t, g.Wait())
	for _, err := range errs {
		require.ErrorContains(t, err, "restores capture")
	}
}

func TestFailEndsTheAttemptForEveryMember(t *testing.T) {
	for _, tc := range []struct {
		name      string
		committed bool
		reason    string
	}{
		{name: "before the attempt commits", reason: ReasonRefused},
		{name: "after the attempt commits", committed: true, reason: session.ReasonMemberFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			members := joinAll(t, 2)
			if tc.committed {
				each(t, members, func(ctx context.Context, m *Member) error { return m.Barrier(ctx, Admitted) })
			}
			waiting := make(chan error, 1)
			go func() { waiting <- members[0].Barrier(within(t), Release) }()

			require.NoError(t, members[1].Fail(within(t)))
			require.ErrorIs(t, <-waiting, &session.AbortError{Reason: tc.reason})
		})
	}
}

// joinAll starts a coordinator with the session of one attempt of n members,
// and joins each of them.
func joinAll(t *testing.T, n int) []*Member {
	t.Helper()
	srv := coordinatortest.Start(t)
	spec := server.Spec{UID: types.UID("attempt-1"), Deadline: time.Now().Add(time.Hour)}
	// Listed out of rank order: a member takes its rank from the session.
	for rank := n - 1; rank >= 0; rank-- {
		spec.Members = append(spec.Members, session.Member{ID: fmt.Sprintf("member-%d", rank), Rank: rank})
	}
	require.NoError(t, srv.Ensure(within(t), spec))

	members := make([]*Member, n)
	for rank := range members {
		m, err := Join(within(t), srv.Config(spec.UID, fmt.Sprintf("member-%d", rank)))
		require.NoError(t, err)
		require.Equal(t, rank, m.Rank())
		t.Cleanup(func() { _ = m.session.Close() })
		members[rank] = m
	}
	return members
}

// each runs op for every member at once, as their agents would, and requires
// that it succeeds for all of them.
func each(t *testing.T, members []*Member, op func(context.Context, *Member) error) {
	t.Helper()
	g, ctx := errgroup.WithContext(within(t))
	for _, m := range members {
		g.Go(func() error { return op(ctx, m) })
	}
	require.NoError(t, g.Wait())
}

func within(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

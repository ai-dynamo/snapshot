// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package group

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/coordinator"
	"github.com/ai-dynamo/snapshot/coordinator/coordinatortest"
	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/ai-dynamo/snapshot/coordinator/session"
)

func TestAdmissionGathersEveryMember(t *testing.T) {
	members := joinAll(t, 3)
	captured := []types.PodGroupManifestMember{
		{ID: "member-0", PodIP: netip.MustParseAddr("10.0.0.1")},
		{ID: "member-1", PodIP: netip.MustParseAddr("10.0.0.2")},
		{ID: "member-2", PodIP: netip.MustParseAddr("10.0.0.3")},
	}
	admissions := make([]Admission, 3)
	for rank := range admissions {
		admissions[rank] = Admission{
			PodIP: netip.MustParseAddr(fmt.Sprintf("10.1.0.%d", rank+1)), PodUID: fmt.Sprintf("pod-%d", rank),
			Containers: []string{"main"}, CaptureID: "capture-1",
		}
	}
	each(t, members, func(ctx context.Context, m *Member) error { return m.Admit(ctx, admissions[m.Rank()]) })

	for _, m := range members {
		require.Equal(t, admissions, m.Admissions())
		require.Equal(t, []string{"member-0", "member-1", "member-2"}, m.MemberIDs())
	}
	peers, err := members[1].PeerIPs(captured)
	require.NoError(t, err)
	require.Equal(t, map[netip.Addr]netip.Addr{
		captured[0].PodIP: admissions[0].PodIP, captured[2].PodIP: admissions[2].PodIP,
	}, peers)
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

func TestAdmissionRefusesAPodIPThatIsNotIPv4(t *testing.T) {
	members := joinAll(t, 1)
	err := members[0].Admit(within(t), Admission{PodIP: netip.MustParseAddr("fd00::1")})
	require.ErrorContains(t, err, "the Pod's IP fd00::1 is not an IPv4 address")

	_, err = members[0].agent.Get(within(t), admitKey(0), 0)
	require.ErrorIs(t, err, session.ErrNotFound, "a refused admission is not published")
}

// A session of the coordinator cannot show how many waits a member has at
// once, so this test counts them in a Store of its own.
func TestAdmissionWaitsTwoAtATimeUntilTheSessionEnds(t *testing.T) {
	deadline := time.Now().Add(time.Hour)
	meta := session.Meta{Deadline: deadline}
	for rank := range 8 {
		meta.Members = append(meta.Members, session.Member{ID: fmt.Sprintf("member-%d", rank), Rank: rank})
	}
	store := &countingStore{}
	m := &Member{session: metaSession{meta: meta}, agent: store, id: "member-0"}

	require.NoError(t, m.Admit(within(t), Admission{PodIP: netip.MustParseAddr("10.1.0.1")}))
	require.Len(t, store.waits, 8)
	require.LessOrEqual(t, store.most, 2)
	for _, wait := range store.waits {
		require.GreaterOrEqual(t, wait, time.Until(deadline)+coordinator.DeadlineSlack)
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

func TestFailAfterAnAdmittedWaitThatEndedEarlyCountsAsCommitted(t *testing.T) {
	members := joinAll(t, 2)
	// Member 0 arrives, then stops waiting before member 1 arrives.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.ErrorIs(t, members[0].Barrier(ctx, Admitted), context.DeadlineExceeded)

	// The arrival stays recorded, so member 1 passes the barrier and starts.
	require.NoError(t, members[1].Barrier(within(t), Admitted))
	waiting := make(chan error, 1)
	go func() { waiting <- members[1].Barrier(within(t), Release) }()

	require.NoError(t, members[0].Fail(within(t)))
	require.ErrorIs(t, <-waiting, &session.AbortError{Reason: session.ReasonMemberFailed})
}

// joinAll starts a coordinator with the session of one attempt of n members,
// and joins each of them.
func joinAll(t *testing.T, n int) []*Member {
	t.Helper()
	srv := coordinatortest.Start(t)
	spec := server.Spec{UID: k8stypes.UID("attempt-1"), Deadline: time.Now().Add(time.Hour)}
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

// metaSession is a member's session of rank 0 that has only its Meta.
type metaSession struct {
	coordinator.Session
	meta session.Meta
}

func (s metaSession) Meta() session.Meta { return s.meta }

func (metaSession) Rank() int { return 0 }

// countingStore answers every Get, after a moment, with an admission, and
// records the wait each Get asked for and the most Gets it answered at once.
type countingStore struct {
	coordinator.Store
	mu            sync.Mutex
	waiting, most int
	waits         []time.Duration
}

func (*countingStore) Insert(context.Context, string, []byte) error { return nil }

func (s *countingStore) Get(_ context.Context, _ string, wait time.Duration) ([]byte, error) {
	s.mu.Lock()
	s.waiting++
	s.most = max(s.most, s.waiting)
	s.waits = append(s.waits, wait)
	s.mu.Unlock()
	time.Sleep(10 * time.Millisecond)
	s.mu.Lock()
	s.waiting--
	s.mu.Unlock()
	return []byte(`{"podIP":"10.1.0.2"}`), nil
}

func within(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

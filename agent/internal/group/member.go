// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package group is the agent's part in a pod group's capture or restore, as
// SNEP-324 defines it. A member joins its attempt's coordination session,
// exchanges admissions with the other members, passes the group's barriers,
// and carries its Pod's cuInterpose relay to the session.
package group

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/coordinator"
	"github.com/ai-dynamo/snapshot/coordinator/session"
)

// The barriers of an attempt, in the order its members pass them. A capture
// passes Admitted and NetFreeze, a restore every other one. SNEP-324's resume
// barrier, which comes before CUDA Checkpoint's resume phase, joins them once
// the agent runs that phase.
const (
	Admitted        = "admitted"
	NetFreeze       = "net-freeze"
	SocketsRestored = "sockets-restored"
	Release         = "release"
)

// ReasonRefused is the reason a member fails an attempt with before it
// commits, which leaves every member as Snapshot found it. A member that fails
// later uses session.ReasonMemberFailed.
const ReasonRefused = "Refused"

// Admission is what a member contributes to the admission exchange, under
// admit.<rank> in the snapshot namespace. Encoding one twice gives the same
// bytes, so publishing it again is harmless.
type Admission struct {
	// PodIP is the member's Pod IP now.
	PodIP netip.Addr `json:"podIP"`
	// PodUID and Containers are set at capture: the UID of the member's Pod
	// and its captured containers, one today, which every member's artifact
	// records for every member.
	PodUID     string   `json:"podUID,omitempty"`
	Containers []string `json:"containers,omitempty"`
	// CaptureID is set at restore: the UID of the PodGroupSnapshot the
	// member's artifact was captured in. Every member must restore the same
	// capture.
	CaptureID string `json:"captureID,omitempty"`
	// RunsRelay reports that the member's Pod runs a cuInterpose coordinator,
	// which takes part in the group as its relay.
	RunsRelay bool `json:"runsRelay,omitempty"`
}

// Member is one member's part in one attempt. The caller joins, hands the
// member to its capture or restore, and leaves once that has succeeded. A
// capture or restore that fails calls Fail, which also closes the connection.
type Member struct {
	session coordinator.Session
	agent   coordinator.Store
	attempt string
	id      string
	// admissions holds every member's admission, by rank, once Admit returns.
	admissions []Admission
	// committed is set once the member calls the Admitted barrier, after which
	// the attempt may have committed: an arrival that was recorded stays
	// recorded even when the call fails, and the other members may then pass
	// the barrier.
	committed atomic.Bool
}

// Join connects to the session of the attempt cfg names, as cfg.Member.
func Join(ctx context.Context, cfg coordinator.Config) (*Member, error) {
	s, err := coordinator.Connect(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("join the session of attempt %s: %w", cfg.Attempt, err)
	}
	return &Member{session: s, agent: s.Snapshot(), attempt: cfg.Attempt, id: cfg.Member}, nil
}

// Attempt returns the UID of the attempt's group resource.
func (m *Member) Attempt() string { return m.attempt }

// ID returns the member's ID.
func (m *Member) ID() string { return m.id }

// Rank returns the member's rank, its position in the group.
func (m *Member) Rank() int { return m.session.Rank() }

// MemberIDs returns the ID of every member of the group, by rank.
func (m *Member) MemberIDs() []string {
	members := m.session.Meta().Members
	ids := make([]string, len(members))
	for rank, member := range members {
		ids[rank] = member.ID
	}
	return ids
}

// Admit publishes own and returns once every member's admission is known. It
// refuses a Pod IP that is not IPv4, since a restore maps IPv4 addresses only,
// and an attempt whose members restore different captures.
func (m *Member) Admit(ctx context.Context, own Admission) error {
	if !own.PodIP.Is4() {
		return fmt.Errorf("the Pod's IP %s is not an IPv4 address, which a pod group needs", own.PodIP)
	}
	value, err := json.Marshal(own)
	if err != nil {
		return err
	}
	if err := m.agent.Insert(ctx, admitKey(m.Rank()), value); err != nil {
		return fmt.Errorf("publish the admission: %w", err)
	}
	meta := m.session.Meta()
	// The session ends at the latest DeadlineSlack past its deadline, so a wait
	// that long returns with how the session ended, rather than racing the
	// coordinator's DeadlineExceeded.
	wait := max(time.Until(meta.Deadline)+coordinator.DeadlineSlack, time.Second)
	admissions := make([]Admission, len(meta.Members))
	g, gctx := errgroup.WithContext(ctx)
	// With the watch of the session's end and a wait of the member's relay, two
	// waits at once stay within the four watches that a member's share of the
	// bucket's consumers allows, as session.MaxConsumersPerMember sizes it.
	g.SetLimit(2)
	for rank := range admissions {
		g.Go(func() error {
			value, err := m.agent.Get(gctx, admitKey(rank), wait)
			if err != nil {
				return fmt.Errorf("read the admission of rank %d: %w", rank, err)
			}
			decoder := json.NewDecoder(bytes.NewReader(value))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&admissions[rank]); err != nil {
				return fmt.Errorf("decode the admission of rank %d: %w", rank, err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	for rank, a := range admissions {
		if a.CaptureID != own.CaptureID {
			return fmt.Errorf("member %s restores capture %q, but member %s restores capture %q",
				m.id, own.CaptureID, meta.Members[rank].ID, a.CaptureID)
		}
	}
	m.admissions = admissions
	return nil
}

// Admissions returns every member's admission, by rank, once Admit returned.
func (m *Member) Admissions() []Admission { return m.admissions }

// PeerIPs maps the IP each other member had at capture to its IP now, from
// captured, the member table a restore reads from its artifact, and the
// admissions.
func (m *Member) PeerIPs(captured []types.PodGroupManifestMember) (map[netip.Addr]netip.Addr, error) {
	if len(captured) != len(m.admissions) {
		return nil, fmt.Errorf("the capture had %d members, the restore has %d", len(captured), len(m.admissions))
	}
	peers := make(map[netip.Addr]netip.Addr, len(captured)-1)
	for rank, a := range m.admissions {
		if rank != m.Rank() {
			peers[captured[rank].PodIP] = a.PodIP
		}
	}
	return peers, nil
}

// Barrier returns once every member has arrived at the barrier name.
func (m *Member) Barrier(ctx context.Context, name string) error {
	if name == Admitted {
		m.committed.Store(true)
	}
	if err := m.session.Barrier(ctx, name); err != nil {
		return fmt.Errorf("barrier %s: %w", name, err)
	}
	return nil
}

// Fail ends the attempt for every member, unless it has ended already, and
// closes the connection. The reason is ReasonRefused until the member calls
// the Admitted barrier, and session.ReasonMemberFailed after, since the
// attempt may then have committed.
func (m *Member) Fail(ctx context.Context) error {
	reason := ReasonRefused
	if m.committed.Load() {
		reason = session.ReasonMemberFailed
	}
	err := m.session.Fail(ctx, reason)
	var abort *session.AbortError
	if errors.As(err, &abort) || errors.Is(err, session.ErrClosed) {
		// The attempt had ended already.
		err = nil
	}
	return errors.Join(err, m.session.Close())
}

// Leave records that this member is done, then keeps the connection until the
// session ends, since a relay or a library may still use it, and closing it
// early would fail the attempt once the grace period passed. It returns once
// the session has ended, or ctx has.
func (m *Member) Leave(ctx context.Context) error {
	err := m.session.Leave(ctx)
	if err == nil {
		select {
		case <-m.session.Done():
			if !errors.Is(m.session.Err(), session.ErrClosed) {
				err = m.session.Err()
			}
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	return errors.Join(err, m.session.Close())
}

func admitKey(rank int) string { return "admit." + strconv.Itoa(rank) }

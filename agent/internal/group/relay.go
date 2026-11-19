// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package group

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/vmihailenco/msgpack/v5"
	"golang.org/x/sys/unix"

	"github.com/ai-dynamo/snapshot/coordinator/session"
)

// Relay is one run of this member's cuInterpose coordinator as its Pod's relay
// in the group. The agent serves one end of a socket pair from the member's
// session, and the coordinator inherits the other.
type Relay struct {
	// File is the coordinator's end, which it inherits as --session-fd.
	File *os.File
	// Args place the coordinator in the group: its rank, the ranks that run a
	// relay, and the attempt's deadline.
	Args   []string
	cancel context.CancelFunc
	served chan error
}

// OpenRelay serves a relay's session socket until Close. Call it once Admit
// has returned. It returns nil for a member whose admission said it runs no
// relay.
func (m *Member) OpenRelay(ctx context.Context) (*Relay, error) {
	if !m.admissions[m.Rank()].RunsRelay {
		return nil, nil
	}
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("create the relay's session socket: %w", err)
	}
	agentEnd := os.NewFile(uintptr(fds[0]), "relay-session-agent")
	conn, err := net.FileConn(agentEnd)
	_ = agentEnd.Close()
	if err != nil {
		_ = unix.Close(fds[1])
		return nil, fmt.Errorf("serve the relay's session socket: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	r := &Relay{
		File:   os.NewFile(uintptr(fds[1]), "relay-session"),
		Args:   m.relayArgs(),
		cancel: cancel,
		served: make(chan error, 1),
	}
	go func() { r.served <- serveRelay(ctx, conn, m) }()
	return r, nil
}

// Close closes this process's copy of the coordinator's end, which ends the
// service once the coordinator has exited too, and waits for that.
func (r *Relay) Close() error {
	_ = r.File.Close()
	err := <-r.served
	r.cancel()
	return err
}

// fabricPeersKey is where the relays' leader records, in the cuinterpose
// namespace, which relays share GPU memory over NVLink, once a capture's
// inspect has validated the group: a MessagePack map from each relay's rank to
// the sorted ranks of the relays it shares memory with, in either direction.
const fabricPeersKey = "inspect.fabric-peers"

// FabricPeers returns the ranks of the members whose Pods share GPU memory
// over NVLink with this member's, as the relays' leader recorded them. Call it
// once this member's relay has inspected.
func (m *Member) FabricPeers(ctx context.Context) ([]int, error) {
	value, err := m.session.CuInterpose().Get(ctx, fabricPeersKey, 0)
	if errors.Is(err, session.ErrNotFound) {
		return nil, fmt.Errorf("the leader of the cuInterpose relays recorded no %s, "+
			"which a cuInterpose older than this agent leaves out", fabricPeersKey)
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", fabricPeersKey, err)
	}
	var shared map[int][]int
	if err := msgpack.Unmarshal(value, &shared); err != nil {
		return nil, fmt.Errorf("decode %s: %w", fabricPeersKey, err)
	}
	own, ok := shared[m.Rank()]
	if !ok {
		return nil, fmt.Errorf("%s has no entry for rank %d", fabricPeersKey, m.Rank())
	}
	for _, peer := range own {
		// A restore reads the peers' admissions by these ranks.
		if peer < 0 || peer >= len(m.admissions) || peer == m.Rank() || !m.admissions[peer].RunsRelay {
			return nil, fmt.Errorf("%s lists rank %d for rank %d, which is not another relay's rank",
				fabricPeersKey, peer, m.Rank())
		}
	}
	return own, nil
}

func (m *Member) relayArgs() []string {
	var ranks []string
	for rank, a := range m.admissions {
		if a.RunsRelay {
			ranks = append(ranks, strconv.Itoa(rank))
		}
	}
	// The relay takes the deadline in whole seconds. Rounded up, it never
	// reaches the deadline before this member does, and past the deadline it
	// waits for the session's end itself.
	deadline := m.session.Meta().Deadline
	seconds := deadline.Unix()
	if deadline.Nanosecond() > 0 {
		seconds++
	}
	return []string{
		"--rank", strconv.Itoa(m.Rank()),
		"--relay-ranks", strings.Join(ranks, ","),
		"--deadline", strconv.FormatInt(seconds, 10),
	}
}

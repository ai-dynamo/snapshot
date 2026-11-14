// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package group

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
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
	go func() { r.served <- serveRelay(ctx, conn, m.session) }()
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

func (m *Member) relayArgs() []string {
	var ranks []string
	for rank, a := range m.admissions {
		if a.RunsRelay {
			ranks = append(ranks, strconv.Itoa(rank))
		}
	}
	return []string{
		"--rank", strconv.Itoa(m.Rank()),
		"--relay-ranks", strings.Join(ranks, ","),
		"--deadline", strconv.FormatInt(m.session.Meta().Deadline.Unix(), 10),
	}
}

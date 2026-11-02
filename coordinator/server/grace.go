// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ai-dynamo/snapshot/coordinator/session"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
)

// followConnections follows members' connections through the server's
// connect and disconnect events, which the server publishes in the system
// account. One subscription carries both, so they are handled in the order
// the server sent them.
func (s *Server) followConnections() error {
	if _, err := s.events.Subscribe("$SYS.ACCOUNT."+appAccount+".*", s.event); err != nil {
		return fmt.Errorf("follow members' connections: %w", err)
	}
	if err := s.events.Flush(); err != nil {
		return fmt.Errorf("follow members' connections: %w", err)
	}
	return nil
}

// event handles one connect or disconnect event. Only an admitted connection
// produces one in the application account, so a refused client cannot pose as
// a member here.
func (s *Server) event(m *nats.Msg) {
	switch {
	case strings.HasSuffix(m.Subject, ".CONNECT"):
		var ev natsserver.ConnectEventMsg
		if err := json.Unmarshal(m.Data, &ev); err == nil {
			s.connected(ev.Client)
		}
	case strings.HasSuffix(m.Subject, ".DISCONNECT"):
		var ev natsserver.DisconnectEventMsg
		if err := json.Unmarshal(m.Data, &ev); err == nil {
			s.disconnected(ev.Client)
		}
	}
}

// member returns the session and member of a client, and false for a client
// that is not a member of an open session. The caller holds Server.mu.
func (s *Server) member(c natsserver.ClientInfo) (*attempt, string, bool) {
	uid, member, ok := session.ParseUserName(c.User)
	if !ok {
		return nil, "", false
	}
	a := s.attempts[types.UID(uid)]
	if a == nil || a.ended || !a.has(member) {
		return nil, "", false
	}
	return a, member, true
}

// connected records a member's connection, which ends its grace period.
func (s *Server) connected(c natsserver.ClientInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, member, ok := s.member(c)
	if !ok {
		return
	}
	if a.conns[member] == nil {
		a.conns[member] = make(map[uint64]struct{})
	}
	a.conns[member][c.ID] = struct{}{}
	if timer := a.grace[member]; timer != nil {
		timer.Stop()
		delete(a.grace, member)
		s.log.Info("Member reconnected within its grace period", "attempt", a.spec.UID, "member", member)
	}
}

// disconnected records that one of a member's connections closed, and starts
// its grace period once it has none left. A member that has left still gets
// one: its relay and its libraries may still use the connection, and losing it
// fails the attempt.
func (s *Server) disconnected(c natsserver.ClientInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, member, ok := s.member(c)
	if !ok {
		return
	}
	delete(a.conns[member], c.ID)
	if len(a.conns[member]) > 0 || a.grace[member] != nil {
		return
	}
	var timer clock.Timer
	timer = s.cfg.Clock.AfterFunc(s.cfg.Grace, func() {
		s.expire(a, session.ReasonMemberFailed, func() bool {
			if a.grace[member] != timer {
				return false
			}
			delete(a.grace, member)
			s.log.Info("Member did not reconnect within its grace period", "attempt", a.spec.UID, "member", member)
			return true
		})
	})
	a.grace[member] = timer
	s.log.Info("Member lost its connection; its grace period started", "attempt", a.spec.UID, "member", member,
		"grace", s.cfg.Grace)
}

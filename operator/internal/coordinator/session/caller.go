// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import "fmt"

// MemberID identifies one member of a set for the length of an attempt.
type MemberID string

// ParticipantID identifies one poster of a barrier. An agent and a relay post
// as their member, while a library posts under the identity of the process
// that called it, because several processes of one member post to the same
// barrier.
type ParticipantID string

// RequestID makes a mutating operation idempotent: the session replays the
// outcome it recorded for the identifier instead of applying the operation
// again. An identifier is scoped to the whole Caller that sent it, so it must
// be unique per caller; in practice the agent assigns every identifier, since
// it carries all of its member's requests. An empty identifier disables the
// replay.
type RequestID string

// Role decides which keys a caller reaches and what of the session's lifecycle
// it may drive. A caller with no role reaches nothing, so an identity
// that was never stamped is refused rather than trusted.
type Role string

// The roles a caller may hold.
const (
	// RoleAgent is a node-level Snapshot agent, acting for one member.
	RoleAgent Role = "agent"

	// RoleRelay is a cuInterpose relay, which an agent starts inside a
	// member's namespaces.
	RoleRelay Role = "relay"

	// RoleLibrary is a workload process speaking a communication library's own
	// rendezvous interface. It reaches no key of Snapshot's own components.
	RoleLibrary Role = "library"
)

// Caller is the identity behind one operation. The layer above this package
// authenticates it and stamps the role; the engine trusts what it is given.
type Caller struct {
	// Member is the member the caller acts for.
	Member MemberID

	// Role decides the caller's namespace and value limit.
	Role Role

	// Library and Version scope a RoleLibrary caller to its own namespace, and
	// are ignored for every other role. A caller of LibraryNCCL carries no
	// version.
	Library string
	Version string
}

// Request is one operation's identity together with, for a mutating operation,
// the identifier that makes a retry idempotent.
type Request struct {
	// Caller is who is asking.
	Caller Caller

	// ID replays the recorded outcome when the same member repeats it. An
	// empty ID applies the operation every time.
	ID RequestID
}

// authorizeAttend reports whether the caller may attach or leave. Only an agent
// does: it holds the session for its member, and a relay reaches the
// coordinator through that agent, so a relay leaving would close the session
// out from under an agent that still needs it.
func (c Caller) authorizeAttend() error {
	if c.Role != RoleAgent {
		return fmt.Errorf("role %q may not attach or leave: %w", c.Role, ErrForbidden)
	}
	return nil
}

// authorizeFail reports whether the caller may fail the session. Snapshot's own
// components may, so a workload process cannot end an attempt it only takes
// part in.
func (c Caller) authorizeFail() error {
	switch c.Role {
	case RoleAgent, RoleRelay:
		return nil
	default:
		return fmt.Errorf("role %q may not fail the session: %w", c.Role, ErrForbidden)
	}
}

// authorizePost reports whether the caller may post to a barrier as
// participant. Snapshot's own components post for their member and nobody
// else, which is what lets a barrier whose count is the number of members mean
// what it says: one agent cannot carry the attempt past admission on its own.
// A library posts under the identity of its process, because several processes
// of one member post to the same barrier.
func (c Caller) authorizePost(participant ParticipantID) error {
	if participant == "" {
		return fmt.Errorf("a post needs a participant: %w", ErrInvalid)
	}
	if len(participant) > MaxKeyLength {
		return fmt.Errorf("a participant of %d bytes is over the %d-byte limit: %w", len(participant), MaxKeyLength,
			ErrTooLarge)
	}
	if c.Role != RoleLibrary && participant != ParticipantID(c.Member) {
		return fmt.Errorf("role %q posts as member %q, not as %q: %w", c.Role, c.Member, participant, ErrForbidden)
	}
	return nil
}

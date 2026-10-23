// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// Member is one member of an attempt.
type Member struct {
	// ID identifies the member.
	ID MemberID

	// Rank is the member's position in the set. Ranks are a permutation of
	// 0 to Size-1, so a value published for one rank has exactly one reader.
	Rank int
}

// Spec is the fixed membership of one attempt, read from its set resource. A
// session never changes it.
type Spec struct {
	// UID is the set resource's UID, which identifies the attempt.
	UID types.UID

	// Members is the whole membership. It must name at least one member, with
	// distinct IDs and ranks covering 0 to len(Members)-1, in any order.
	Members []Member

	// Deadline bounds the whole attempt, counted from the resource's creation.
	Deadline time.Time
}

// validate reports whether the spec describes an attempt a session can run.
func (s Spec) validate() error {
	if s.UID == "" {
		return fmt.Errorf("no UID: %w", ErrInvalid)
	}
	if len(s.Members) == 0 {
		return fmt.Errorf("no members: %w", ErrInvalid)
	}
	if s.Deadline.IsZero() {
		return fmt.Errorf("no deadline: %w", ErrInvalid)
	}
	byID := make(map[MemberID]struct{}, len(s.Members))
	byRank := make(map[int]struct{}, len(s.Members))
	for _, m := range s.Members {
		if m.ID == "" {
			return fmt.Errorf("member without an ID: %w", ErrInvalid)
		}
		if _, dup := byID[m.ID]; dup {
			return fmt.Errorf("member %q named twice: %w", m.ID, ErrInvalid)
		}
		if m.Rank < 0 || m.Rank >= len(s.Members) {
			return fmt.Errorf("member %q has rank %d outside 0..%d: %w", m.ID, m.Rank, len(s.Members)-1, ErrInvalid)
		}
		if _, dup := byRank[m.Rank]; dup {
			return fmt.Errorf("rank %d taken twice: %w", m.Rank, ErrInvalid)
		}
		byID[m.ID] = struct{}{}
		byRank[m.Rank] = struct{}{}
	}
	return nil
}

// byRank returns the membership of a validated spec ordered by rank, which is
// the one order two specs of the same attempt are compared in.
func (s Spec) byRank() []Member {
	members := make([]Member, len(s.Members))
	for _, m := range s.Members {
		members[m.Rank] = m
	}
	return members
}

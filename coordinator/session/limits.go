// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

// The limits a session's buckets have unless the coordinator is configured
// otherwise. Each bucket limits the size of its values, and the library bucket
// also its number of keys; no bucket reserves memory, and every session shares
// the coordinator's memory cap. MaxValue is SNEP-324's default, to revisit once
// the largest cuInterpose record has been measured on the largest supported
// topology. Meta carries the value limits a session actually has, and the
// client refuses a larger value itself.
const (
	// MaxValue is the largest value the Snapshot bucket holds: agent and relay
	// records, which can describe a whole topology.
	MaxValue = 32 << 20

	// MaxLibraryValue is the largest value the library bucket holds. NCCL's
	// communicator identifiers take 128 bytes.
	MaxLibraryValue = 4 << 10

	// MaxLibraryKeys is how many keys the library bucket holds, deleted ones
	// included, which keeps one session's library values under 16 MiB. With
	// keys and headers, a library bucket reaches about 22 MiB.
	MaxLibraryKeys = 4096
)

// MaxConsumersPerMember sizes how many consumers each of a session's buckets
// holds: one limit per bucket, of this many for each member, which the
// session's members share, so one member may hold more than its share while
// others hold less. The limit bounds the memory consumers take. A member past
// it waits for a consumer to free: the client retries a watch the limit
// refuses until its caller's context or wait ends.
//
// Every watch is an ordered consumer, and a reconnect replaces each one with a
// new consumer while the old one is still being deleted, so each member's
// share is twice what it watches at once. On the Snapshot bucket, the client
// watches the session's end, and also for each Barrier in progress and each
// Get with a wait of a key that is absent: four watches at once allow a
// Barrier, and a Get by the agent and one by its cuInterpose relay. On the
// library bucket, the agent's library endpoint waits through Get once per key
// per node, for the keys its Pod's libraries ask for at once, such as NCCL's
// communicator identifiers.
const MaxConsumersPerMember = 8

// MaxNameLength bounds an attempt UID, a member ID, and a barrier name.
const MaxNameLength = 253

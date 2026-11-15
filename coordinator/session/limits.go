// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

// The limits a session's buckets have unless the coordinator is configured
// otherwise. The Snapshot and library buckets live in memory: each limits the
// size of its values, the library bucket also its number of keys, none reserves
// memory, and every session shares the coordinator's memory cap. The control
// bucket lives in file storage, outside that cap, and has a size of its own:
// see ControlBytes. MaxValue is SNEP-324's default, to revisit once
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

// The control bucket holds only keys the session needs to end, so its size
// follows the membership. It is bounded so that a member cannot fill the
// coordinator's file storage, and sized so that a session of honest members
// never reaches the bound.
const (
	// ControlBytesPerMember is what each member adds to its control bucket's
	// size: its share of Meta, its departure, and its arrivals at about a
	// hundred barriers.
	ControlBytesPerMember = 16 << 10

	// ControlBytesBase is what every control bucket has beyond its members: the
	// ended key, and a margin.
	ControlBytesBase = 64 << 10

	// MaxControlValue is the largest value the control bucket holds, apart from
	// Meta, which is as large as its membership needs. A reason takes at most
	// 1 KiB; arrivals and departures hold nothing.
	MaxControlValue = 4 << 10
)

// ControlBytes is the size of the control bucket of a session of members
// members, in bytes. The coordinator's file storage holds the control buckets
// of every session, so it bounds them in total by their sum.
func ControlBytes(members int) int64 { return ControlBytesBase + int64(members)*ControlBytesPerMember }

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
// watches each Get with a wait of a key that is absent: two watches at once
// allow a Get by the agent and one by its cuInterpose relay. On the control
// bucket, it watches the session's end, and also each Barrier in progress: two
// watches at once. On the library bucket, the agent's library endpoint waits
// through Get once per key per node, for the keys its Pod's libraries ask for at once, such as NCCL's
// communicator identifiers.
const MaxConsumersPerMember = 8

// MaxConcurrentGets is how many values a client reads at once over its
// connection, and how many of the largest values the coordinator buffers for a
// connection before it drops the connection as a slow consumer. The client
// reads no more at once than the coordinator holds, so concurrent Gets never
// cost a reconnect.
const MaxConcurrentGets = 2

// MaxNameLength bounds a member ID and a barrier name.
const MaxNameLength = 253

// maxStreamName is the longest name JetStream gives a stream.
const maxStreamName = 255

// MaxAttemptLength bounds an attempt UID, so the longest name JetStream gives
// one of the attempt's buckets, the stream that holds it, stays within
// JetStream's limit. Kubernetes UIDs take 36 bytes.
const MaxAttemptLength = maxStreamName - len("KV_"+bucketPrefix+"-s")

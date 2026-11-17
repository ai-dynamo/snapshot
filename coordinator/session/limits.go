// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

// The limits a session's buckets have unless the coordinator is configured
// otherwise. The Snapshot and library buckets live in memory: each limits the
// size of its values, the library bucket also its number of keys, none reserves
// memory, and every session shares the coordinator's memory cap. The control
// bucket lives in file storage, outside that cap, and has a size of its own:
// see ControlBytes. MaxValue also bounds configured values so concurrent reads
// fit the client's shared reply subscription. Meta carries the value limits a
// session actually has, and the client refuses a larger value itself.
const (
	// MaxValue is the largest value the Snapshot bucket holds: agent and relay
	// records, which can describe a whole topology. Concurrent replies must fit
	// the client's shared reply subscription, including their framing.
	MaxValue = nats.DefaultSubPendingBytesLimit/MaxConcurrentGets - PayloadHeadroom

	// MaxLibraryValue is the largest value the library bucket holds. NCCL's
	// communicator identifiers take 128 bytes.
	MaxLibraryValue = 4 << 10

	// MaxLibraryKeys is how many keys the library bucket holds, deleted ones
	// included, which keeps one session's library values under 16 MiB. With
	// keys and headers, a library bucket reaches about 22 MiB.
	MaxLibraryKeys = 4096
)

// The control bucket holds only keys the session needs to end, so its size
// follows the membership: see ControlBytes. It is bounded so that a member
// cannot fill the coordinator's file storage, and sized so that a session of
// honest members never reaches the bound.
const (
	// ArrivalsPerMember is how many barrier arrivals the control bucket holds
	// for each member. An attempt's agents arrive at a handful of barriers;
	// sixteen leaves room for more.
	ArrivalsPerMember = 16

	// MaxControlValue is the largest value a control key holds, apart from
	// Meta: the longest reason, which the ended key holds. Arrivals and
	// departures hold nothing.
	MaxControlValue = maxReasonLength
)

// recordFraming is what the coordinator's file store adds to each record
// beyond its subject, headers and value, in bytes: the record's length,
// sequence, timestamp, subject length, header length, and checksum.
const recordFraming = 4 + 8 + 8 + 2 + 4 + 8

// MaxMembers is the largest membership a session is sized for: the most
// members a PodGroupSnapshot or PodGroupRestore holds.
const MaxMembers = 256

// ControlBytes is the size of the control bucket of a session of members
// members, in bytes: the worst case of what the session writes there, which
// is each member's arrivals and departure, the session's end, and Meta. The
// coordinator's file storage holds the control buckets of every session, so
// it bounds them in total by their sum.
func ControlBytes(members int) int64 {
	records := int64(members)*(ArrivalsPerMember+1) + 1
	return records*int64(maxControlRecord()) + int64(metaRecord(members))
}

// maxControlRecord is the most a record of the control bucket takes, apart
// from Meta's: its framing, the longest subject, which an arrival with the
// longest attempt UID, barrier name and member ID has, and the largest value.
// The ended key's creation is the one write that carries headers, and its
// subject is shorter than an arrival's by more than they take.
func maxControlRecord() int {
	name := strings.Repeat("n", MaxNameLength)
	return recordFraming + len(Subject(ControlBucket(longestAttempt()), BarrierKey(name, name))) + MaxControlValue
}

// metaRecord is the most Meta's record takes in the control bucket of a
// session of members members.
func metaRecord(members int) int {
	return recordFraming + len(Subject(ControlBucket(longestAttempt()), KeyMeta)) + MaxMetaBytes(members)
}

// ControlValueBytes is the largest value the control bucket of a session of
// members members holds: the largest Meta, or MaxControlValue.
func ControlValueBytes(members int) int { return max(MaxControlValue, MaxMetaBytes(members)) }

// longestAttempt returns the longest attempt UID ValidateAttempt accepts.
func longestAttempt() string { return strings.Repeat("u", MaxAttemptLength) }

// MaxMetaBytes is the size of the largest Meta of a session of members
// members, encoded: every member with the longest ID and the widest rank, and
// the widest deadline, epoch and limits.
func MaxMetaBytes(members int) int {
	meta := Meta{
		Members: make([]Member, members),
		// The latest time JSON holds, at the widest UTC offset.
		Deadline:        time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.FixedZone("", -(23*60*60+59*60))),
		Epoch:           math.MaxUint64,
		MaxValue:        math.MaxInt,
		MaxLibraryValue: math.MaxInt,
	}
	id := strings.Repeat("m", MaxNameLength)
	for i := range meta.Members {
		meta.Members[i] = Member{ID: id, Rank: members - 1}
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		panic(fmt.Sprintf("encode the largest Meta: %v", err))
	}
	return len(encoded)
}

// MaxConsumersPerMember sizes how many consumers each of a session's buckets
// holds: one limit per bucket, of this many for each member, which the
// session's members share, so one member may hold more than its share while
// others hold less. The limit bounds the memory consumers take. A member past
// it waits for a consumer to free: the client retries a watch the limit
// refuses, waiting longer each time, until its caller's context or wait ends.
//
// Eight is twice four, the most keys a member is sized to watch at once on one
// bucket. Every watch is an ordered consumer, and a reconnect replaces each one
// with a new consumer while the old one is still being deleted, so a member's
// share is twice what it watches at once. On the control bucket, one watch
// follows barrier arrivals and the session's end together. On the Snapshot
// bucket, it watches each Get with a wait of a key that is absent, such as one
// by the agent and one by its cuInterpose relay: two. On the library bucket,
// the agent's library endpoint waits through Get for the keys its Pod's
// libraries ask for at once, such as NCCL's communicator identifiers: up to
// four.
const MaxConsumersPerMember = 8

// What the coordinator buffers for a connection, and how a client reads
// within it: see MaxPending.
const (
	// MaxConcurrentGets is how many of the Snapshot bucket's largest values a
	// client reads at once over its connection.
	MaxConcurrentGets = 2

	// PayloadHeadroom is what the coordinator's largest message may carry
	// beyond the largest value of any bucket: the headers of a write, and
	// what a read's reply adds, which ReplyHeadroom bounds. The key in a
	// write's subject does not count.
	PayloadHeadroom = 64 << 10

	// ReplyHeadroom bounds what a read's reply carries beyond its value: the
	// headers the value was written with, the headers the coordinator adds,
	// which name the bucket's stream and repeat the key's subject, and the
	// reply's own subject, which is the member's inbox. With the longest names
	// and key, they take under 3 KiB.
	ReplyHeadroom = 8 << 10
)

// MaxPayload is the largest message the coordinator accepts, for a Snapshot
// bucket whose largest value is maxValue: the largest value of any bucket, and
// PayloadHeadroom. That value is maxValue, or the largest Meta of a session of
// MaxMembers members when maxValue is smaller.
func MaxPayload(maxValue int) int {
	return max(maxValue, ControlValueBytes(MaxMembers)) + PayloadHeadroom
}

// MaxPending is how many bytes the coordinator buffers for a connection, of
// what it has sent and the connection has not taken yet, before it drops the
// connection as a slow consumer: MaxConcurrentGets of its largest messages,
// MaxPayload, for a Snapshot bucket whose largest value is maxValue. A client
// budgets its reads within it, in bytes, each the largest value of its bucket
// and ReplyHeadroom, so concurrent Gets never cost a reconnect, and a small
// read goes on beside the largest ones.
func MaxPending(maxValue int) int64 { return MaxConcurrentGets * int64(MaxPayload(maxValue)) }

// MaxNameLength bounds a member ID and a barrier name.
const MaxNameLength = 253

// maxStreamName is the longest name JetStream gives a stream.
const maxStreamName = 255

// MaxAttemptLength bounds an attempt UID, so the longest name JetStream gives
// one of the attempt's buckets, the stream that holds it, stays within
// JetStream's limit. Kubernetes UIDs take 36 bytes.
const MaxAttemptLength = maxStreamName - len("KV_"+bucketPrefix+"-s")

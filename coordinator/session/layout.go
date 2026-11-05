// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// LayoutVersion versions the bucket names and the key layout below. It is part
// of every bucket name, so a client and a coordinator that disagree on the
// layout never share a bucket.
const LayoutVersion = "v1"

// bucketPrefix starts every bucket name.
const bucketPrefix = "snapshot-" + LayoutVersion + "-"

// SnapshotBucket names the bucket of an attempt that Snapshot's own components
// use: the agents and the cuInterpose relays.
func SnapshotBucket(attempt string) string { return bucketPrefix + attempt + "-s" }

// LibraryBucket names the bucket of an attempt that workload libraries use,
// through the agent's per-Pod endpoint.
func LibraryBucket(attempt string) string { return bucketPrefix + attempt + "-l" }

// The keys of the Snapshot bucket that the session itself uses. The Snapshot
// bucket, SnapshotBucket(attempt), serves Snapshot's components:
//
//	meta                     the Meta, written by the coordinator only
//	ended                    how the session ended, created once
//	barrier.<name>.<member>  a member's arrival, written by its own connection
//	left.<member>            a member's departure, written by its own connection
//	snapshot.<key>           the agents' keys
//	cuinterpose.<key>        the cuInterpose relays' keys
//
// The library bucket, LibraryBucket(attempt), holds library.<key> keys, which
// any workload library writes through the agent's library endpoint.
const (
	// KeyMeta holds the session's Meta, as JSON. Only the coordinator writes it.
	KeyMeta = "meta"

	// KeyEnded holds how the session ended: the reason it failed, or no bytes
	// at all when it closed because every member left. It is only ever
	// created, so the first outcome wins. Every member's connection may write
	// it, so a misbehaving member can overwrite or delete it within its own
	// attempt, which is no worse than failing the session through Fail.
	KeyEnded = "ended"

	// barrierPrefix starts each member's arrival at a barrier:
	// barrier.<name>.<member>.
	barrierPrefix = "barrier."

	// leftPrefix starts each member's report that it is done: left.<member>.
	leftPrefix = "left."
)

// BarrierKey is the key of member's arrival at the barrier name. A "*" for
// either matches every barrier or every member.
func BarrierKey(name, member string) string { return barrierPrefix + name + "." + member }

// LeftKey is the key a member writes when it leaves. A "*" matches every
// member.
func LeftKey(member string) string { return leftPrefix + member }

// Namespace is one client's part of a session. The agent reaches all three, and
// hands the relay only the cuInterpose namespace, and a workload's libraries
// only the library namespace, which confines them.
type Namespace string

// The namespaces of a session. Each is also the first token of its keys.
const (
	// NamespaceSnapshot holds the agents' keys, in the Snapshot bucket.
	NamespaceSnapshot Namespace = "snapshot"

	// NamespaceCuInterpose holds the cuInterpose relays' keys, in the Snapshot
	// bucket.
	NamespaceCuInterpose Namespace = "cuinterpose"

	// NamespaceLibrary holds workload libraries' keys, in the library bucket.
	// The agent's library endpoint serves any library and cannot tell which
	// one sent a key, so the namespace names none.
	NamespaceLibrary Namespace = "library"
)

// nameRE matches one key token that is safe in a user name and an inbox
// subject too.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ValidateName reports ErrInvalid unless s can name a member or a barrier: a
// single key token of letters, digits, '-' and '_', of at most MaxNameLength
// bytes. Object names without dots qualify.
func ValidateName(s string) error {
	if len(s) > MaxNameLength || !nameRE.MatchString(s) {
		return fmt.Errorf("%q is not a name of letters, digits, '-' and '_', of at most %d bytes: %w",
			s, MaxNameLength, ErrInvalid)
	}
	return nil
}

// ValidateAttempt reports ErrInvalid unless s can name an attempt: a name, as
// ValidateName defines it, of at most MaxAttemptLength bytes. Kubernetes UIDs
// qualify.
func ValidateAttempt(s string) error {
	if len(s) > MaxAttemptLength {
		return fmt.Errorf("an attempt of %d bytes is over the %d-byte limit: %w", len(s), MaxAttemptLength, ErrInvalid)
	}
	return ValidateName(s)
}

// UserName is the user name a member's connection presents and is registered
// under, which the coordinator maps the connection's events back to the member
// with. Its password carries the epoch the member last read from Meta, in
// decimal, and is empty until the member has read one.
func UserName(attempt, member string) string { return attempt + "/" + member }

// ParseUserName splits a user name made by UserName. It reports false for any
// other user name, such as the coordinator's own.
func ParseUserName(user string) (attempt, member string, ok bool) {
	attempt, member, ok = strings.Cut(user, "/")
	if !ok || ValidateName(attempt) != nil || ValidateName(member) != nil {
		return "", "", false
	}
	return attempt, member, true
}

// InboxPrefix is the prefix of a member's reply subjects. It is the member's
// own, so no other connection can subscribe to the replies it receives.
func InboxPrefix(attempt, member string) string { return "_INBOX." + attempt + "." + member }

// Permissions lists the subjects a member's connection may publish to and
// subscribe to, which the coordinator grants when it admits the connection.
type Permissions struct {
	// Publish lists the subjects the connection may publish to.
	Publish []string

	// Subscribe lists the subjects the connection may subscribe to.
	Subscribe []string
}

// MemberPermissions returns the permissions of a member's connection: both of
// its attempt's buckets and nothing else, never another attempt's buckets, the
// system account, or the API that creates and deletes buckets. In the Snapshot
// bucket it may write the snapshot and cuinterpose namespaces, the ended key,
// and its own arrivals and its own departure, never meta or another member's
// barrier or left key; in the library bucket it may write anything. Its
// JetStream API requests are limited to what a client needs to bind a bucket,
// Get a key, and Watch keys, and the only subscriptions it may make are on its
// own inbox, where replies and watched entries arrive.
//
// The permissions do not bound where a consumer the member creates delivers
// (nats-server#8274), so a member can add messages to another attempt's
// buckets under its own subjects, which count against that attempt's limits
// but never change its keys. SNEP-324 lists this with the memory cap every
// session shares.
func MemberPermissions(attempt, member string) Permissions {
	snapshot, library := SnapshotBucket(attempt), LibraryBucket(attempt)
	publish := make([]string, 0, 16)
	for _, bucket := range []string{snapshot, library} {
		stream := "KV_" + bucket
		publish = append(publish,
			"$JS.API.STREAM.INFO."+stream,          // binding the bucket
			"$JS.API.DIRECT.GET."+stream+".>",      // Get
			"$JS.API.CONSUMER.CREATE."+stream+".>", // Watch, through an ordered consumer
			"$JS.API.CONSUMER.DELETE."+stream+".*", // stopping a watch
			"$JS.FC."+stream+".>",                  // a watch's flow control replies
		)
	}
	kv := "$KV." + snapshot + "."
	publish = append(publish,
		kv+string(NamespaceSnapshot)+".>",
		kv+string(NamespaceCuInterpose)+".>",
		kv+KeyEnded,
		kv+BarrierKey("*", member),
		kv+LeftKey(member),
		"$KV."+library+".>",
	)
	return Permissions{Publish: publish, Subscribe: []string{InboxPrefix(attempt, member) + ".>"}}
}

// Member is one member of an attempt.
type Member struct {
	// ID identifies the member. It is a name, as ValidateName defines.
	ID string `json:"id"`

	// Rank is the member's position among the attempt's members. The ranks of
	// an attempt are a permutation of 0 to len(members)-1.
	Rank int `json:"rank"`
}

// Meta is what a member reads once when it connects: the attempt's fixed
// membership and deadline, the epoch of the coordinator instance holding the
// session, and the value limits of its buckets.
type Meta struct {
	// Members is the whole membership, ordered by rank, so Members[i].Rank is
	// i.
	Members []Member `json:"members"`

	// Deadline bounds the whole attempt.
	Deadline time.Time `json:"deadline"`

	// Epoch is drawn at random, never zero, each time the coordinator creates
	// the session, so a member that reconnects can tell a session the
	// coordinator recreated after a restart from the one it joined.
	Epoch uint64 `json:"epoch"`

	// MaxValue is the largest value the Snapshot bucket holds.
	MaxValue int `json:"maxValue"`

	// MaxLibraryValue is the largest value the library bucket holds.
	MaxLibraryValue int `json:"maxLibraryValue"`
}

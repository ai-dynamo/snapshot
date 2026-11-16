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

// ControlBucket names the bucket of an attempt that holds the session's own
// keys: how it started, who arrived and left, and how it ended. It lives in
// file storage, which the coordinator's memory cap does not limit, so a session
// can always end however full that cap is.
func ControlBucket(attempt string) string { return bucketPrefix + attempt + "-c" }

// SnapshotBucket names the bucket of an attempt that Snapshot's own components
// use: the agents and the cuInterpose relays.
func SnapshotBucket(attempt string) string { return bucketPrefix + attempt + "-s" }

// LibraryBucket names the bucket of an attempt that workload libraries use,
// through the agent's per-Pod endpoint.
func LibraryBucket(attempt string) string { return bucketPrefix + attempt + "-l" }

// Buckets lists the buckets of an attempt.
func Buckets(attempt string) []string {
	return []string{ControlBucket(attempt), SnapshotBucket(attempt), LibraryBucket(attempt)}
}

// Subject is the subject a write of key in bucket publishes to, and its
// record in the bucket's stream has.
func Subject(bucket, key string) string { return "$KV." + bucket + "." + key }

// The keys of the control bucket, ControlBucket(attempt), which the session
// itself uses:
//
//	meta                     the Meta, written by the coordinator only
//	ended                    how the session ended, created once
//	barrier.<name>.<member>  a member's arrival, written by its own connection
//	left.<member>            a member's departure, written by its own connection
//
// The Snapshot bucket, SnapshotBucket(attempt), serves Snapshot's components:
//
//	snapshot.<key>           the agents' keys
//	cuinterpose.<key>        the cuInterpose relays' keys
//
// The library bucket, LibraryBucket(attempt), holds library.<key> keys, which
// any workload library writes through the agent's library endpoint.
// Namespace.Bucket maps each namespace to its bucket.
const (
	// KeyMeta holds the session's Meta, as JSON. Only the coordinator writes it.
	KeyMeta = "meta"

	// KeyEnded holds how the session ended: the reason it failed, or no bytes
	// at all when it closed because every member left. It is only ever
	// created, so the first outcome wins. Every member's connection may write
	// it, so a misbehaving member can overwrite or delete it within its own
	// attempt, which is no worse than failing the session through Fail. It can
	// also fill the control bucket with arrivals, so that no end fits; the
	// coordinator keeps trying to record the end, and the host removes the
	// session once its deadline has passed.
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

// namespaces maps every namespace to the bucket that holds its keys. It is
// the one place that does, so the client, the coordinator and the member
// permissions agree.
var namespaces = []struct {
	ns     Namespace
	bucket func(attempt string) string
}{
	{NamespaceSnapshot, SnapshotBucket},
	{NamespaceCuInterpose, SnapshotBucket},
	{NamespaceLibrary, LibraryBucket},
}

// Namespaces lists every namespace.
func Namespaces() []Namespace {
	all := make([]Namespace, len(namespaces))
	for i, n := range namespaces {
		all[i] = n.ns
	}
	return all
}

// Bucket names the bucket of an attempt that holds the namespace's keys. It
// reports ErrInvalid for a namespace that Namespaces does not list.
func (n Namespace) Bucket(attempt string) (string, error) {
	for _, m := range namespaces {
		if m.ns == n {
			return m.bucket(attempt), nil
		}
	}
	return "", fmt.Errorf("no namespace %q: %w", n, ErrInvalid)
}

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

// MemberPermissions returns the permissions of a member's connection: its
// attempt's buckets and nothing else, never another attempt's buckets, the
// system account, or the API that creates and deletes buckets. In the control
// bucket it may write the ended key, its own arrivals and its own departure,
// never meta or another member's barrier or left key; it may write each
// namespace's keys in the bucket Namespace.Bucket names. Its JetStream API
// requests are limited to what a client needs to bind a bucket, Get a key,
// and Watch keys, and the only subscriptions it may make are on its own inbox,
// where replies and watched entries arrive.
//
// The permissions guard against an agent's mistakes, not a compromised agent:
// every agent is equally trusted, as package server states. They do not bound
// a request's reply subject, through which an agent could write any key in the
// shared account. Nor do they bound where a consumer the member creates
// delivers (nats-server#8274), so a member can add messages to another
// attempt's buckets under its own subjects, which count against that attempt's
// limits. Such a delivery keeps its own subject, so it never changes a key.
func MemberPermissions(attempt, member string) Permissions {
	buckets := Buckets(attempt)
	controlKeys := []string{KeyEnded, BarrierKey("*", member), LeftKey(member)}
	publish := make([]string, 0, len(buckets)*len(bucketAPI)+len(controlKeys)+len(namespaces))
	for _, bucket := range buckets {
		for _, api := range bucketAPI {
			publish = append(publish, api.prefix+"KV_"+bucket+api.suffix)
		}
	}
	for _, key := range controlKeys {
		publish = append(publish, Subject(ControlBucket(attempt), key))
	}
	for _, n := range namespaces {
		publish = append(publish, Subject(n.bucket(attempt), string(n.ns)+".>"))
	}
	return Permissions{Publish: publish, Subscribe: []string{InboxPrefix(attempt, member) + ".>"}}
}

// bucketAPI lists the JetStream API subjects a member's connection may publish
// to for each of its attempt's buckets, around the name of the bucket's
// stream.
var bucketAPI = []struct{ prefix, suffix string }{
	{"$JS.API.STREAM.INFO.", ""},       // binding the bucket
	{"$JS.API.DIRECT.GET.", ".>"},      // Get
	{"$JS.API.CONSUMER.CREATE.", ".>"}, // Watch, through an ordered consumer
	{"$JS.API.CONSUMER.DELETE.", ".*"}, // stopping a watch
	// A watch's flow control replies, which nats.go sends whenever the server
	// asks, once a value past its threshold has been delivered. Refusing one
	// logs a violation on both sides, and stalls a watch that then receives a
	// second large value.
	{"$JS.FC.", ".>"},
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

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package server is SNEP-324's Snapshot coordinator: it serves each
// attempt's coordination session from a NATS server with JetStream embedded in
// the process. It owns the server, the sessions, and authentication, and
// nothing else. It shares only package session
// (github.com/ai-dynamo/snapshot/coordinator/session) with the session client:
// the bucket names and key layout, the default limits, the user name a member
// connects under, the subjects its connection is permitted, Meta, and the
// errors. The host, whatever process embeds the server, sits above this
// package: it ensures and removes sessions, and provides the Authorizer.
//
// # The embedded server
//
// The server keeps every bucket in memory only, as a single replica, with no
// cluster, no HTTP monitoring port, and its log routed into the coordinator's
// with key-value subjects redacted. It has two accounts. Members and the
// coordinator's own connection share the application account, the only one
// with JetStream. The system account carries the server's connect and
// disconnect events, which a second in-process connection follows. Both
// in-process connections authenticate with a token drawn at startup, which
// never leaves the process's memory. The client listener uses TLS.
//
// Each bucket limits the size of its values and how many consumers it holds,
// which grows with the number of members, and the library bucket also its
// number of keys. No bucket reserves memory. Every session shares the memory
// cap, Config.MaxBytes, so a session that fills it makes other sessions' writes
// fail with session.ErrTooMany until it is removed.
//
// # Authentication
//
// Every client goes through the server's custom authentication. A member
// presents its user name, session.UserName(uid, member), the agent's
// ServiceAccount token, and as its password the epoch it last read, if any.
// The coordinator refuses every member until MarkReady, then asks the
// Authorizer, then checks the attempt's session and its membership, and
// admits the member with session.MemberPermissions: both of the attempt's
// buckets, and in the Snapshot bucket only its own arrivals and departure
// besides the shared keys, never meta, another attempt's buckets, the system
// account, or the API that creates and deletes buckets. A member can still add
// messages to another attempt's buckets under its own subjects, through a
// consumer whose deliveries its permissions do not bound (nats-server#8274),
// which count against that attempt's limits but never change its keys.
// SNEP-324 lists this with the memory cap every session shares.
//
// # Sessions
//
// The host drives sessions, level-triggered: Ensure while an attempt's
// set resource is unfinished, and Remove once it is deleted, or once it is
// final and its session has ended. A session ends exactly once, through the
// ended key, which is only ever created: by a member's Fail; by the
// coordinator when every member has left, when the deadline passes, when a
// member's grace period runs out, when it removes a session still open, and
// when it shuts down; or by a member that finds the coordinator lost the
// session it joined. Once the deadline passes or a grace period runs out, the
// coordinator keeps trying to record the end, which the memory cap may refuse,
// until it is recorded, the session is removed, or the coordinator shuts down.
//
// # The grace period
//
// The coordinator follows every member's connections through the connect and
// disconnect events, by user name. When a member has no connection left, its
// grace period starts, and a new connection before it runs out ends it. Once
// it runs out while the session is open, the session fails with
// session.ReasonMemberFailed, even if the member connects again before the
// end is recorded. A member
// that has left still gets one, since its relay and its libraries keep using
// the connection until the session ends.
//
// # Restarts and the epoch
//
// A coordinator restart loses every session. The host ensures them again with
// new epochs before it calls MarkReady, and a member that reconnects presents
// the epoch it read: a different one makes the coordinator fail the recreated
// session with session.ReasonCoordinatorLost before admitting the member, so
// the member never writes to a session it did not join. The member itself then
// reads meta, finds the other epoch, and reports session.ErrCoordinatorLost.
package server

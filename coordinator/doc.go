// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package coordinator is the client of SNEP-324's coordination sessions.
// Package session holds what it shares with the Snapshot coordinator: the
// layout, the limits, Meta, and the errors every operation reports.
//
// The coordinator keeps each session in a NATS server with JetStream that it
// embeds; this package hides NATS entirely, and no NATS type appears in its
// API. Each member's agent connects once per hosted member with Connect, and
// drives the session's six patterns: Insert, Get, and Delete on a namespace's
// Store, and Barrier, Leave, and Fail on the Session.
//
// # Waits and the session's end
//
// Waits are pushed, never polled: a waiting Get and a Barrier watch their keys
// together with ended, so a wait returns the moment its key is inserted or its
// barrier releases, and returns how the session ended as soon as it ends. The
// session ends exactly once: it closes when every member has left, or fails
// with the first reason anyone gives, a member through Fail or the coordinator
// at the deadline, when a member's connection stays lost past the grace
// period, when it removes a session still open, and when it shuts down. A
// member that cannot reach the coordinator to learn the end ends its session
// itself, with session.ReasonDeadlineExceeded, DeadlineSlack past the
// deadline.
//
// # Reconnection and the epoch
//
// The client reconnects on its own, without limit and with short waits, until
// the session ends or is closed. Watches resume from the revision they had
// reached, and every operation is safe to send again, which the session does
// itself for an operation that was not answered. The coordinator's grace
// period covers a connection that breaks while the agent and the coordinator
// keep running: the client notices a dead connection, and connects again,
// within it.
//
// The coordinator keeps sessions only in memory, so a restart loses them, and
// it recreates the session of every unfinished attempt with a new epoch before
// it admits any client. It also deletes a session's buckets once it removes
// the session, whether or not every member has received its end. A member
// presents the epoch it read from Meta whenever it connects, and verifies its
// session, by reading Meta again, whenever the session may be gone: after a
// reconnect, when a watch stops on its own, and when an operation finds its
// bucket missing. Another epoch, or no session, means the coordinator no
// longer holds what the member had, so the session ends with
// session.ErrCoordinatorLost. The coordinator fails a session it recreated,
// with session.ReasonCoordinatorLost, before it admits a member that presents
// another epoch, so every other member learns at once.
package coordinator

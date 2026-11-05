// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package session is the contract between SNEP-324's session client and the
// Snapshot coordinator, and everything the two share: the layout of a session's
// buckets, the limits the coordinator applies, the user name and permissions
// of a member's connection, the Meta a member reads, and the errors either side
// reports. It imports no NATS package. The client is package coordinator, at
// this module's root, and the coordinator is package server.
//
// A session is one attempt of a set capture or restore, identified by the set
// resource's UID. The coordinator keeps it in two key-value buckets of the NATS
// server with JetStream that it embeds.
//
// # Layout
//
// The Snapshot bucket, SnapshotBucket(uid), serves Snapshot's components:
//
//	meta                     the Meta, written by the coordinator only
//	ended                    how the session ended, created once
//	barrier.<name>.<member>  a member's arrival, written by its own connection
//	left.<member>            a member's departure, written by its own connection
//	snapshot.<key>           the agents' keys
//	cuinterpose.<key>        the cuInterpose relays' keys
//
// The library bucket, LibraryBucket(uid), holds library.<key> keys, which any
// workload library writes through the agent's library endpoint. Bucket names
// carry LayoutVersion.
//
// # Limits
//
// Each bucket limits the size of its values and how many consumers it holds,
// which grows with the number of members and which the session's members
// share, and the library bucket also its number of keys. No bucket reserves memory. Every session shares the
// coordinator's memory cap, so a session that fills it makes other sessions'
// writes fail with ErrTooMany until it is removed. Meta carries the value
// limits a session actually has.
package session

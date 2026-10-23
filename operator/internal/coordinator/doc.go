// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package coordinator serves the Snapshot coordinator's sessions to agents over
// gRPC, with the protocol of package coordinatorv1alpha1.
//
// The server owns the protocol and nothing else: the handshake, multiplexing
// requests on a stream, cancellation, reattaching, the grace period, the
// failure event, and translating between wire messages and the session layer.
// Every session rule, from membership and namespaces to the byte budget,
// replay, deadlines, and terminal states, stays in package session, which the
// server calls and never duplicates. Storage sits below that, in package kv.
//
// # Streams
//
// Each stream carries one member's part in one attempt. Its handler reads the
// Hello, asks the Authorizer, and admits the stream to the session; one
// goroutine then receives messages while the handler dispatches them, and each
// request runs in a goroutine of its own, which then sends its response. The
// agent bounds how many requests it forwards; a stream that leaves far more
// unanswered, running or waiting to be sent, ends, as a backstop. The receive
// goroutine returns as soon as gRPC ends the stream, which happens when the
// handler returns.
//
// Sends are serialized, because gRPC forbids concurrent sends on one stream,
// and a send blocks while the client is not reading. A session's Done channel
// closes only once nothing is left running in it, so a stream that sees it
// waits for every response to go out and then sends Ended last. It keeps
// receiving until Ended has gone out, so a client that gives the stream up, or
// breaks it, still ends it. A stream that is lost or superseded instead waits
// only for its requests to stop running: it sends nothing more, and gRPC
// interrupts a blocked send once the handler returns, so a client that stopped
// reading cannot hold up the member's grace period.
//
// # Registry
//
// The server tracks the stream each member is attached through, and the grace
// period of each member whose stream was lost, under one mutex. Streams are
// numbered as their Hellos arrive, and a member's newest stream wins: one whose
// handshake finishes after a newer stream registered, such as after a slow
// authorization, never displaces it. The server never calls the manager, a
// session, or the clock while holding that mutex, because the clock may run a
// grace callback, which takes it, under a lock of its own. For the same reason
// the callback fails an expired member's session from a short-lived goroutine:
// failing a session stops its deadline timer, which such a clock forbids from
// inside a callback.
package coordinator

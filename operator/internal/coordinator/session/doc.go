// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package session holds Snapshot's rules for the coordinator's sessions: who
// takes part in an attempt, what each caller may reach and store, how retries
// replay, and how an attempt ends.
//
// A session is one attempt of a set capture or restore, identified by the set
// resource's UID. Its membership is fixed: members with contiguous ranks, and a
// deadline that bounds the whole attempt. Members attach, exchange small values
// through write-once keys and named barriers, and leave. A session has exactly
// two outcomes. It closes once every member has left, or it fails with a
// reason, after which every pending and every later operation reports an error
// matching ErrAborted. Nothing here knows which steps of an attempt are
// destructive; a member's own durable record decides what a failure does to it.
//
// # Layers
//
// Storage sits below this package, behind the small interfaces of package kv: a
// Store of per-attempt spaces, each offering values, waits, and barriers. A
// session reaches its keys and barriers only through kv.Space, so a backend
// such as Redis could replace the in-memory one without changing anything here.
// Everything Snapshot decides stays in this package: callers, roles, and
// namespaces; value limits and the byte budget; request replay; who may attach,
// leave, fail, and post as whom; the deadline, the terminal states, and the
// Observer. The gRPC service, the authentication that stamps a caller's role,
// and the reconciler that follows the set resources all sit above it.
//
// # Lifecycle
//
// The set resources drive the lifecycle, level-triggered. A reconciler calls
// Manager.Ensure while an attempt's resource is unfinished, and Manager.Remove
// once the resource is deleted, or once it is final and its session has ended,
// which Session.Done and Session.Err report. A final resource is not enough on
// its own: a restore is final once every member is released, while NCCL may
// still be rebuilding its communicators through the session. The service finds
// sessions with Manager.Session.
//
// A session that has ended stays until Remove, without its keys or barriers,
// so a member that arrives late, whether to attach again or with a request
// still in flight, is told how the attempt ended. The deadline fails a session
// still open when it passes, which also ends a session whose agents never come
// back, and Manager.Shutdown fails every session when the coordinator
// terminates.
//
// # Who takes part
//
// Every operation names a member, and a caller the attempt does not name is
// refused, so only a member's own agent, relay, and workload processes take
// part in it. Attaching and leaving are an agent's alone, since a relay reaches
// the coordinator through its agent. A member's agent leaving does not cut the
// member off: its relay and its workload processes keep reading and writing
// until the session closes. Failing the session is open to an agent and a
// relay, never to a workload process, so a workload cannot end an attempt it
// only takes part in. An agent and a relay post to a barrier only as their own
// member, so a barrier counting members counts each member once.
//
// # Namespaces and the budget
//
// Every key and every barrier name lives in the namespace of the caller's
// role, so a workload process can neither read nor forge the keys of Snapshot's
// own components:
//
//	RoleAgent    snapshot/
//	RoleRelay    cuinterpose/
//	RoleLibrary  cuda/<library>/<version>/, or nccl/ for the NCCL Checkpoint
//	             compatibility endpoint, whose clients carry no version
//
// Values are opaque bytes, limited to MaxLibraryValue in a library namespace
// and to MaxValue elsewhere, and every key, barrier name, and participant to
// MaxKeyLength. Everything a session holds, whoever writes it, is charged to
// one byte budget, MaxSessionBytes: keys and values, barriers, participants,
// and the requests it records for replay. A flood from a workload process can
// exhaust its own session's budget, which at worst fails that attempt, never
// another one.
//
// # Retries
//
// Every mutating operation may carry a RequestID. The session records that
// request's outcome for its lifetime and replays it when the same caller
// repeats the identifier, so a retried Increment does not apply twice; a retry
// that arrives while the original still runs waits for its outcome. An empty
// identifier disables the replay, and the operation applies every time.
//
// # Aborts and concurrency
//
// Each session has a context, cancelled with the terminal error as its cause
// when the session ends. Every call into the space runs under a context joined
// to it, so an abort reaches a pending Get or AwaitBarrier through ordinary
// cancellation, and the call reports the cause. After cancelling, the session
// waits for every call still inside the space to return, then drops the space,
// so a backend never sees a call on a space it has dropped. Barriers release
// once and stay released.
//
// The package starts no goroutine of its own: a waiter blocks in its caller's
// goroutine, and the deadline is a clock callback. A session never calls its
// clock or its Observer while holding a lock of its own, and never calls its
// clock while one of its calls is counted inside the space, so a clock may run
// the deadline under a lock of its own. The Manager arms each deadline under
// its mutex, which no clock callback takes.
package session

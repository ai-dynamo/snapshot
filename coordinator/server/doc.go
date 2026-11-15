// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package server is SNEP-324's Snapshot coordinator: it serves each attempt's
// coordination session from a NATS server with JetStream embedded in the
// process, and authenticates every client. The host, the process that embeds
// it, ensures, cancels, and removes sessions and provides the Authorizer, under
// the rules Ensure, Cancel, Remove, and MarkReady state; package groups does
// that from the group resources. Sessions live in memory, except their control
// keys, which use a small file store that does not outlive the process, so a
// full memory cap never stops a session from ending. Package session holds
// what the coordinator shares with the session client, package coordinator.
//
// Every member the coordinator admits is an agent, and every agent is equally
// trusted. A member's permissions keep an agent from changing what is not its
// own by mistake. They do not stop a compromised agent, which could write any
// key in the shared account through a request's reply subject. Isolating agents
// from each other is a beta item, with narrowing the agent's Pod permissions
// and isolating sessions' memory.
package server

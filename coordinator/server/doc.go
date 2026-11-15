// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package server is SNEP-324's Snapshot coordinator: it serves each attempt's
// coordination session from a NATS server with JetStream embedded in the
// process, and authenticates every client. The host, the process that embeds
// it, ensures and removes sessions and provides the Authorizer, under the rules
// Ensure, Remove, and MarkReady state. Sessions live in memory, except their
// control keys, which use a small file store that does not outlive the process,
// so a full memory cap never stops a session from ending. Package session holds
// what the coordinator shares with the session client, package coordinator.
package server

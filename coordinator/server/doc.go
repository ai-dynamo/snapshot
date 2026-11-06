// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package server is SNEP-324's Snapshot coordinator: it serves each attempt's
// coordination session from a NATS server with JetStream embedded in the
// process, and authenticates every client. The host, the process that embeds
// it, ensures, cancels, and removes sessions and provides the Authorizer, under
// the rules Ensure, Cancel, Remove, and MarkReady state; package groups does
// that from the group resources. Package session holds what the coordinator
// shares with the session client, package coordinator.
package server

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package coordinator is the session client of SNEP-324's coordination
// sessions: the Snapshot agent connects with Connect once for each member it
// hosts. No NATS type appears in its API. Package session holds what it shares
// with the coordinator, package server.
package coordinator

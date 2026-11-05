// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package session is what SNEP-324's session client, package coordinator, and
// the Snapshot coordinator, package server, share: the layout of a session's
// buckets, their limits, the user name and permissions of a member's
// connection, Meta, and the errors either side reports. It imports no NATS
// package.
package session

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import "time"

// MaxKeyLength bounds a key as a Store's caller passes it, before its namespace
// is added and, for a library key, before it is encoded.
const MaxKeyLength = 1024

// The client's connection to the coordinator. A client whose coordinator stops
// answering gives up on the connection after ClientPingInterval ×
// (ClientMaxPingsOut + 1), then connects again within ClientConnectTimeout,
// and both together fit within the coordinator's grace period.
const (
	// ClientPingInterval is how often a client pings the coordinator.
	ClientPingInterval = 3 * time.Second

	// ClientMaxPingsOut is how many pings a client leaves unanswered before it
	// drops the connection and reconnects.
	ClientMaxPingsOut = 2

	// ClientConnectTimeout bounds each connection a client makes, the first
	// and every reconnect: dialing, the TLS handshake, and the coordinator's
	// authentication, which answers within it.
	ClientConnectTimeout = 5 * time.Second
)

// DeadlineSlack is how long past Meta.Deadline a member waits to learn how its
// session ended before it ends the session itself, with
// session.ReasonDeadlineExceeded. The coordinator fails the session at the
// deadline, by its own clock, which may run behind the member's, so a member
// that has not learned the end by then cannot reach the coordinator.
const DeadlineSlack = 5 * time.Second

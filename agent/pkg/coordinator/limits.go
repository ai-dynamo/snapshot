// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import "time"

// MaxKeyLength bounds a key as a Store's caller passes it, before its namespace
// is added and, for a library key, before it is encoded.
const MaxKeyLength = 1024

// The client's keepalive. A client whose coordinator stops answering gives up
// on the connection after ClientPingInterval × (ClientMaxPingsOut + 1), at most
// half the coordinator's grace period, which leaves it the rest to reconnect.
const (
	// ClientPingInterval is how often a client pings the coordinator.
	ClientPingInterval = 3 * time.Second

	// ClientMaxPingsOut is how many pings a client leaves unanswered before it
	// drops the connection and reconnects.
	ClientMaxPingsOut = 2
)

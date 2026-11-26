// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sessiontest

import (
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator"
	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/stretchr/testify/assert"
)

// TestConnectionBudget ties the client's connection settings to the
// coordinator's. The grace period starts once the coordinator sees a member's
// connection close; within it, the client must notice the dead connection,
// which takes ClientPingInterval × (ClientMaxPingsOut + 1), and connect again,
// which takes at most ClientConnectTimeout. The coordinator must authenticate
// a connection within AuthorizeTimeout, before the client gives up on it and
// connects again.
func TestConnectionBudget(t *testing.T) {
	detection := coordinator.ClientPingInterval * time.Duration(coordinator.ClientMaxPingsOut+1)
	assert.Less(t, detection+coordinator.ClientConnectTimeout, server.DefaultGrace,
		"the client notices a dead connection after %s and connects again within %s, within a %s grace period",
		detection, coordinator.ClientConnectTimeout, server.DefaultGrace)
	assert.Less(t, server.AuthorizeTimeout, coordinator.ClientConnectTimeout,
		"the coordinator authenticates a connection within %s, before the client gives up after %s",
		server.AuthorizeTimeout, coordinator.ClientConnectTimeout)
}

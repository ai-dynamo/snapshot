// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sessiontest

import (
	"testing"
	"time"

	client "github.com/ai-dynamo/snapshot/agent/pkg/coordinator"
	"github.com/ai-dynamo/snapshot/operator/internal/coordinator"
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
	detection := client.ClientPingInterval * time.Duration(client.ClientMaxPingsOut+1)
	assert.Less(t, detection+client.ClientConnectTimeout, coordinator.DefaultGrace,
		"the client notices a dead connection after %s and connects again within %s, within a %s grace period",
		detection, client.ClientConnectTimeout, coordinator.DefaultGrace)
	assert.Less(t, coordinator.AuthorizeTimeout, client.ClientConnectTimeout,
		"the coordinator authenticates a connection within %s, before the client gives up after %s",
		coordinator.AuthorizeTimeout, client.ClientConnectTimeout)
}

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

// TestClientKeepaliveFitsTheGracePeriod ties the client's keepalive to the
// coordinator's grace period, which starts once the coordinator sees a
// member's connection close. A client gives up on a dead connection after
// ClientPingInterval × (ClientMaxPingsOut + 1), and must do so well within the
// grace period to leave itself the time to reconnect.
func TestClientKeepaliveFitsTheGracePeriod(t *testing.T) {
	detection := client.ClientPingInterval * time.Duration(client.ClientMaxPingsOut+1)
	assert.LessOrEqual(t, detection, coordinator.DefaultGrace/2,
		"the client notices a dead connection after %s, within a %s grace period", detection,
		coordinator.DefaultGrace)
}

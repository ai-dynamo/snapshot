// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"bytes"
	"strings"
	"testing"

	wire "github.com/ai-dynamo/snapshot/agent/pkg/coordinator/v1alpha1"
	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestMaxMessageSizeHoldsTheLargestRequest(t *testing.T) {
	largest := origin{&wire.Origin{Role: wire.Role_ROLE_LIBRARY,
		Library: strings.Repeat("l", session.MaxKeyLength), Version: strings.Repeat("v", session.MaxKeyLength)}}.
		compareAndSwap(strings.Repeat("i", session.MaxKeyLength), strings.Repeat("k", session.MaxKeyLength),
			make([]byte, session.MaxValue+1), make([]byte, session.MaxValue+1))
	msg := &wire.ClientMessage{Message: &wire.ClientMessage_Request{Request: largest}}

	assert.LessOrEqual(t, proto.Size(msg), wire.MaxMessageSize,
		"a compare-and-swap of two values a byte over the limit still travels, to be refused in-band")
}

func TestTheLargestValuesCrossTheWire(t *testing.T) {
	f := newFixture(t)
	f.ensure(t)
	c, _ := f.attach(t, memberA, 0)
	largest := bytes.Repeat([]byte{0xa5}, session.MaxValue)
	replacement := bytes.Repeat([]byte{0x5a}, session.MaxValue)

	c.request(t, asAgent().insert("insert", agentKey, largest))
	c.ok(t, "insert")
	c.request(t, asAgent().get("get", agentKey, 0))
	require.Equal(t, largest, c.ok(t, "get").GetValue())
	c.request(t, asAgent().compareAndSwap("swap", agentKey, largest, replacement))
	c.ok(t, "swap")
	c.request(t, asAgent().insert("over", agentOther, append(largest, 0)))

	c.failure(t, "over", wire.Code_CODE_TOO_LARGE)
}

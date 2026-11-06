// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package groups

import (
	"testing"

	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/stretchr/testify/require"
)

// TestSetupRequires covers what Setup requires, which it checks before it
// touches the manager: the attempts, the server, and the node.
func TestSetupRequires(t *testing.T) {
	attempts, srv := &Attempts{}, &server.Server{}
	for name, tt := range map[string]struct {
		attempts *Attempts
		cfg      Config
	}{
		"no attempts": {nil, Config{Server: srv, Node: "node"}},
		"no server":   {attempts, Config{Node: "node"}},
		"no node":     {attempts, Config{Server: srv}},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, Setup(nil, tt.attempts, tt.cfg), session.ErrInvalid)
		})
	}
	require.NoError(t, Config{Server: srv, Node: "node"}.validate(), "Clock defaults to the real clock")
}

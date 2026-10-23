// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv/memory"
	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/clock"
)

func TestNewServerNeedsAManagerAndAnAuthorizer(t *testing.T) {
	manager, err := session.NewManager(session.Config{Store: memory.New()})
	require.NoError(t, err)
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "no manager", cfg: Config{Authorizer: &authorizer{}}},
		{name: "no authorizer", cfg: Config{Manager: manager}},
		{name: "a negative grace period", cfg: Config{Manager: manager, Authorizer: &authorizer{}, Grace: -time.Second}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewServer(tt.cfg)

			require.Error(t, err)
		})
	}
}

func TestNewServerFillsInTheDefaults(t *testing.T) {
	manager, err := session.NewManager(session.Config{Store: memory.New()})
	require.NoError(t, err)

	s, err := NewServer(Config{Manager: manager, Authorizer: &authorizer{}})

	require.NoError(t, err)
	assert.Equal(t, DefaultGrace, s.registry.grace)
	assert.Equal(t, clock.RealClock{}, s.registry.clock)
}

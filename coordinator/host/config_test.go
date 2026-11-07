// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"testing"

	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
)

// validConfig returns a Config that validates.
func validConfig() Config {
	return Config{
		RESTConfig:     &rest.Config{Host: "https://127.0.0.1:6443"},
		Node:           "node-0",
		ServiceAccount: types.NamespacedName{Namespace: "snapshot-system", Name: "snapshot-agent"},
		Audience:       "snapshot-coordinator",
		Host:           "10.0.0.1",
		Port:           4224,
		HealthPort:     4225,
		TLSDir:         "/etc/snapshot-coordinator/tls",
	}
}

// TestRunValidatesConfig covers what Run checks before it opens anything,
// including that it listens on one address only.
func TestRunValidatesConfig(t *testing.T) {
	for name, modify := range map[string]func(*Config){
		"no REST config":              func(c *Config) { c.RESTConfig = nil },
		"no node":                     func(c *Config) { c.Node = "" },
		"no ServiceAccount namespace": func(c *Config) { c.ServiceAccount.Namespace = "" },
		"no ServiceAccount name":      func(c *Config) { c.ServiceAccount.Name = "" },
		"no audience":                 func(c *Config) { c.Audience = "" },
		"no TLS directory":            func(c *Config) { c.TLSDir = "" },
		"no address":                  func(c *Config) { c.Host = "" },
		"a host name":                 func(c *Config) { c.Host = "localhost" },
		"every IPv4 interface":        func(c *Config) { c.Host = "0.0.0.0" },
		"every IPv6 interface":        func(c *Config) { c.Host = "::" },
		"no port":                     func(c *Config) { c.Port = 0 },
		"a port too large":            func(c *Config) { c.Port = 65536 },
		"no health port":              func(c *Config) { c.HealthPort = 0 },
		"a negative health port":      func(c *Config) { c.HealthPort = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig()
			modify(&cfg)
			assert.ErrorIs(t, Run(t.Context(), cfg), session.ErrInvalid)
		})
	}
	t.Run("valid", func(t *testing.T) {
		require.NoError(t, validConfig().validate())
		cfg := validConfig()
		cfg.Host = "fd00::1"
		require.NoError(t, cfg.validate(), "an IPv6 address")
	})
}

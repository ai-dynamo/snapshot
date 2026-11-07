// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"io"
	"testing"

	"github.com/ai-dynamo/snapshot/coordinator/host"
	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

// parse parses args with the coordinator's flags.
func parse(t *testing.T, args ...string) (host.Config, error) {
	t.Helper()
	fs := flag.NewFlagSet("snapshot-coordinator", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfg := bindFlags(fs)
	err := fs.Parse(args)
	return *cfg, err
}

// required are the flags without a default, which the chart sets from the
// agent Pod.
var required = []string{
	"--node-name=node-0",
	"--agent-namespace=snapshot-system",
	"--agent-service-account=snapshot-agent",
	"--token-audience=snapshot-coordinator",
	"--listen-address=10.0.0.1",
	"--tls-cert-dir=/etc/snapshot-coordinator/tls",
}

func TestFlags(t *testing.T) {
	want := host.Config{
		Node:           "node-0",
		ServiceAccount: types.NamespacedName{Namespace: "snapshot-system", Name: "snapshot-agent"},
		Audience:       "snapshot-coordinator",
		Host:           "10.0.0.1",
		TLSDir:         "/etc/snapshot-coordinator/tls",
	}

	t.Run("the defaults", func(t *testing.T) {
		cfg, err := parse(t, required...)
		require.NoError(t, err)
		want := want
		want.Port, want.HealthPort = 4224, 4225
		want.MaxBytes = server.DefaultMaxBytes
		want.MaxValue, want.MaxLibraryValue = session.MaxValue, session.MaxLibraryValue
		want.MaxLibraryKeys = session.MaxLibraryKeys
		assert.Equal(t, want, cfg)
	})

	t.Run("every flag", func(t *testing.T) {
		cfg, err := parse(t, append(required,
			"--port=5001",
			"--health-port=5002",
			"--max-memory-bytes=2147483648",
			"--max-value-bytes=1048576",
			"--max-library-value-bytes=1024",
			"--max-library-keys=128",
		)...)
		require.NoError(t, err)
		want := want
		want.Port, want.HealthPort = 5001, 5002
		want.MaxBytes = 2 << 30
		want.MaxValue, want.MaxLibraryValue = 1<<20, 1<<10
		want.MaxLibraryKeys = 128
		assert.Equal(t, want, cfg)
	})

	t.Run("no metrics flag", func(t *testing.T) {
		_, err := parse(t, "--metrics-bind-address=:8080")
		require.Error(t, err)
	})
}

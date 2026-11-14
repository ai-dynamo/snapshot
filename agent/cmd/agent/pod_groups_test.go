// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ai-dynamo/snapshot/agent/internal/group"
)

func parsePodGroupFlags(t *testing.T, args ...string) *podGroupConfig {
	t.Helper()
	flags := flag.NewFlagSet("agent", flag.ContinueOnError)
	cfg := bindPodGroupFlags(flags)
	require.NoError(t, flags.Parse(args))
	return cfg
}

func TestPodGroupFlagsAreOffByDefault(t *testing.T) {
	cfg := parsePodGroupFlags(t)
	assert.False(t, cfg.enabled, "no flag is required while pod groups are off")
	assert.Equal(t, group.DefaultPort, cfg.port)
}

func TestPodGroupFlagsBuildTheLocator(t *testing.T) {
	cfg := parsePodGroupFlags(t, "--enable-pod-groups", "--agent-namespace=snapshot-system",
		"--agent-selector=app.kubernetes.io/component=snapshot-agent,app.kubernetes.io/instance=snapshot",
		"--coordinator-server-name=snapshot-coordinator.snapshot-system", "--coordinator-port=4300")
	locator, err := cfg.locator(logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, "snapshot-system", locator.Namespace)
	assert.Equal(t, "app.kubernetes.io/component=snapshot-agent,app.kubernetes.io/instance=snapshot", locator.Selector.String())
	assert.Equal(t, "snapshot-coordinator.snapshot-system", locator.ServerName)
	assert.Equal(t, 4300, locator.Port)
	assert.Empty(t, locator.CAFile, "the chart's paths are the default")
}

func TestPodGroupFlagsRefuseAnIncompleteConfiguration(t *testing.T) {
	complete := []string{"--enable-pod-groups", "--agent-namespace=snapshot-system",
		"--agent-selector=app.kubernetes.io/component=snapshot-agent", "--coordinator-server-name=coordinator"}
	for name, tc := range map[string]struct {
		drop  string
		extra string
		want  string
	}{
		"no namespace":   {drop: complete[1], want: "--agent-namespace is required"},
		"no selector":    {drop: complete[2], want: "--agent-selector is required"},
		"no server name": {drop: complete[3], want: "--coordinator-server-name is required"},
		"a bad selector": {extra: "--agent-selector=a in (", want: "parse --agent-selector"},
		"a bad port":     {extra: "--coordinator-port=0", want: "is not a port"},
	} {
		t.Run(name, func(t *testing.T) {
			var args []string
			for _, arg := range complete {
				if arg != tc.drop {
					args = append(args, arg)
				}
			}
			if tc.extra != "" {
				args = append(args, tc.extra)
			}
			_, err := parsePodGroupFlags(t, args...).locator(logr.Discard())
			require.ErrorContains(t, err, tc.want)
		})
	}
}

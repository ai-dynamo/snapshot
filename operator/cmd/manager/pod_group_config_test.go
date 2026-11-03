// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/labels"
)

func TestPodGroupFlags(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name: "valid",
			args: []string{"--enable-pod-groups=true", "--agent-namespace=snapshot-system",
				"--agent-selector=app.kubernetes.io/component=snapshot-agent,app.kubernetes.io/instance=snapshot"},
		},
		{
			name:    "no namespace",
			args:    []string{"--enable-pod-groups=true", "--agent-selector=app=agent"},
			wantErr: "--agent-namespace is required with --enable-pod-groups",
		},
		{
			name:    "no selector",
			args:    []string{"--enable-pod-groups=true", "--agent-namespace=snapshot-system"},
			wantErr: "--agent-selector is required with --enable-pod-groups",
		},
		{
			name:    "an invalid selector",
			args:    []string{"--enable-pod-groups=true", "--agent-namespace=snapshot-system", "--agent-selector=a b"},
			wantErr: "parse --agent-selector",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flags := flag.NewFlagSet("test", flag.ContinueOnError)
			cfg := bindPodGroupFlags(flags)
			require.NoError(t, flags.Parse(tc.args))
			require.True(t, cfg.enabled)

			selector, err := cfg.selector()
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.True(t, selector.Matches(labels.Set{
				"app.kubernetes.io/component": "snapshot-agent", "app.kubernetes.io/instance": "snapshot",
			}))
			assert.False(t, selector.Matches(labels.Set{
				"app.kubernetes.io/component": "snapshot-agent", "app.kubernetes.io/instance": "other",
			}), "another release's agents are not this release's")
		})
	}
}

func TestPodGroupFlagsOffByDefault(t *testing.T) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg := bindPodGroupFlags(flags)
	require.NoError(t, flags.Parse(nil))
	assert.False(t, cfg.enabled)
}

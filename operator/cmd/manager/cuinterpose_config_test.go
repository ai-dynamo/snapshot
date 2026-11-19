// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func parseCuInterposeFlags(t *testing.T, args ...string) error {
	t.Helper()
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	preparation := bindCuInterposeFlags(flags)
	webhook := bindCuInterposeWebhookFlags(flags)
	require.NoError(t, flags.Parse(args))
	if err := preparation.Validate(); err != nil {
		return err
	}
	return webhook.Validate(*preparation)
}

func TestCuInterposeWebhookFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "disabled by default"},
		{
			name: "enabled with init container preparation",
			args: []string{"--multi-gpu-preparation=initContainer", "--cuinterpose-image=registry.example/agent:v1",
				"--cuinterpose-webhook", "--cuinterpose-webhook-cert-dir=/certs"},
		},
		{
			name: "enabled with agent preparation",
			args: []string{"--cuinterpose-webhook", "--cuinterpose-webhook-cert-dir=/certs"},
			want: `cuinterpose-webhook needs --multi-gpu-preparation=initContainer, got "agent"`,
		},
		{
			name: "missing certificate directory",
			args: []string{"--multi-gpu-preparation=initContainer", "--cuinterpose-image=registry.example/agent:v1",
				"--cuinterpose-webhook"},
			want: "cuinterpose-webhook-cert-dir is required",
		},
		{
			name: "port out of range",
			args: []string{"--multi-gpu-preparation=initContainer", "--cuinterpose-image=registry.example/agent:v1",
				"--cuinterpose-webhook", "--cuinterpose-webhook-cert-dir=/certs", "--cuinterpose-webhook-port=70000"},
			want: "between 1 and 65535",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := parseCuInterposeFlags(t, tc.args...)
			if tc.want == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tc.want)
		})
	}
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRunSubcommandHelpSucceeds(t *testing.T) {
	for _, args := range [][]string{
		{"checkpoint", "--help"},
		{"checkpoint", "-h"},
		{"restore", "--help"},
		{"restore", "-h"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			require.NoError(t, run(args))
		})
	}
}

func TestRunSubcommandRejectsUnknownFlag(t *testing.T) {
	for _, subcommand := range []string{"checkpoint", "restore"} {
		t.Run(subcommand, func(t *testing.T) {
			err := run([]string{subcommand, "--no-such-flag"})
			require.ErrorContains(t, err, "flag provided but not defined")
		})
	}
}

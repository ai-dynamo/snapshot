// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

// CRIU reports a full volume only in its message text, so the errno every
// caller tests for has to be put back on the error here. CRIU capitalises the
// message and the errno does not, which is why the match ignores case.
func TestWithFullDiskErrno(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "a full volume is re-reported as ENOSPC", want: true,
			err: errors.New("CRIU dump failed: Error (criu/page-xfer.c:752): page-xfer: " +
				"Unable to splice data: No space left on device")},
		{name: "any other dump failure is left alone",
			err: errors.New("CRIU dump failed: Unable to dump pid 1")},
		{name: "no error stays no error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := withFullDiskErrno(tc.err)
			if tc.err == nil {
				require.NoError(t, got)
				return
			}
			require.ErrorIs(t, got, tc.err, "the cause must stay reachable")
			assert.Equal(t, tc.want, errors.Is(got, unix.ENOSPC))
		})
	}
}

// go-criu folds a git build's sublevel into the minor number and throws the
// commit away, so what CRIU states is read from the binary instead. The commit
// is what identifies a build from a moving branch, which the agent defaults to.
func TestParseVersion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
		want   string
	}{
		{
			name:   "a build from git is named by its commit",
			output: "Version: 4.3\nGitID: v4.3-128-g1a2b3c4d\n",
			want:   "4.3 (v4.3-128-g1a2b3c4d)",
		},
		{
			name:   "a release states no commit and needs none",
			output: "Version: 4.3\n",
			want:   "4.3",
		},
		{
			name:   "a sublevel is reported as CRIU states it",
			output: "Version: 3.19.1\nGitID: v3.19.1\n",
			want:   "3.19.1 (v3.19.1)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseVersion(tc.output)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	_, err := parseVersion("criu: command not found")
	assert.Error(t, err, "output with no version is an error, not an empty version")

	_, err = parseVersion("Version:\nGitID: v4.3-128-g1a2b3c4d\n")
	assert.Error(t, err, "a field is read from its own line, never the next one")
}

// Both callers resolve the configured binary the same way, so the version the
// agent reports at startup names the binary its dumps run.
func TestResolveBinary(t *testing.T) {
	dir := t.TempDir()
	onPath := filepath.Join(dir, DefaultBinary)
	require.NoError(t, os.WriteFile(onPath, []byte("#!/bin/sh\n"), 0o755))
	t.Setenv("PATH", dir)

	for _, tc := range []struct {
		name     string
		settings *types.CRIUSettings
		want     string
	}{
		{name: "no configuration takes the default off PATH", want: onPath},
		{name: "a bare name is taken off PATH", want: onPath,
			settings: &types.CRIUSettings{BinaryPath: DefaultBinary}},
		{name: "a path is taken as given", want: onPath,
			settings: &types.CRIUSettings{BinaryPath: onPath}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveBinary(tc.settings)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	_, err := ResolveBinary(&types.CRIUSettings{BinaryPath: filepath.Join(dir, "no-such-criu")})
	assert.Error(t, err, "a binary that is not there is not usable")

	notExecutable := filepath.Join(dir, "unrunnable")
	require.NoError(t, os.WriteFile(notExecutable, nil, 0o644))
	_, err = ResolveBinary(&types.CRIUSettings{BinaryPath: notExecutable})
	assert.Error(t, err, "a file that cannot be executed is not usable either")
}

// A binary that cannot be asked leaves the version unknown rather than failing
// the dump that was already failing.
func TestVersionOrUnknown(t *testing.T) {
	got := versionOrUnknown(context.Background(),
		&types.CRIUSettings{BinaryPath: filepath.Join(t.TempDir(), "no-such-criu")})
	assert.Equal(t, "unknown", got)
}

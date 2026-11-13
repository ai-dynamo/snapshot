// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
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

// CRIU packs its version into one integer, and a bug report quoting "40100" is
// no more use than one quoting nothing.
func TestFormatCriuVersion(t *testing.T) {
	for encoded, want := range map[int]string{
		40000:  "4.0.0",
		40100:  "4.1.0",
		301502: "30.15.2",
		31207:  "3.12.7",
	} {
		assert.Equal(t, want, formatCriuVersion(encoded))
	}
}

// The default CRIU reference is a development branch, where every commit
// reports the same version number. The commit the image was built from is what
// makes a dev build identifiable at all.
func TestWithBuildRef(t *testing.T) {
	dir := t.TempDir()
	recorded := filepath.Join(dir, "criu-build-ref")
	require.NoError(t, os.WriteFile(recorded, []byte("1a2b3c4d5e6f\n"), 0o644))

	assert.Equal(t, "4.3.0 (1a2b3c4d5e6f)", withBuildRef("4.3.0", recorded),
		"a recorded commit identifies the build")
	assert.Equal(t, "4.3.0", withBuildRef("4.3.0", filepath.Join(dir, "absent")),
		"a release build is identified by its version alone")

	empty := filepath.Join(dir, "empty")
	require.NoError(t, os.WriteFile(empty, []byte("  \n"), 0o644))
	assert.Equal(t, "4.3.0", withBuildRef("4.3.0", empty), "an empty file adds nothing")
}

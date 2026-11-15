// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"errors"
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

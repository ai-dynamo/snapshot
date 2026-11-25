// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package executor

import (
	"errors"
	"slices"
	"testing"

	"github.com/go-logr/logr"
	"golang.org/x/sys/unix"
)

func TestUnmountShmForCRIU(t *testing.T) {
	testCases := []struct {
		name      string
		strictErr error // error from the flags=0 unmount; nil means success
		detachErr error // error from the MNT_DETACH unmount; nil means success
		wantCalls []int // flags of each unmount call, in order
		wantErr   error // nil expects success; otherwise the error must wrap this
	}{
		{
			name:      "strict unmount succeeds",
			wantCalls: []int{0},
		},
		{
			name:      "busy mount falls back to lazy unmount",
			strictErr: unix.EBUSY,
			wantCalls: []int{0, unix.MNT_DETACH},
		},
		{
			name:      "busy mount and failed lazy unmount",
			strictErr: unix.EBUSY,
			detachErr: unix.EPERM,
			wantCalls: []int{0, unix.MNT_DETACH},
			wantErr:   unix.EPERM,
		},
		{
			name:      "other error is not retried",
			strictErr: unix.ENOENT,
			wantCalls: []int{0},
			wantErr:   unix.ENOENT,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var calls []int
			ops := shmUnmountOps{
				unmount: func(path string, flags int) error {
					if path != "/dev/shm" {
						t.Errorf("unmount path = %q, want /dev/shm", path)
					}
					calls = append(calls, flags)
					if len(calls) == 1 {
						return tc.strictErr
					}
					return tc.detachErr
				},
			}

			err := unmountShmForCRIU(ops, logr.Discard())

			if !slices.Equal(calls, tc.wantCalls) {
				t.Errorf("unmount calls = %v, want %v", calls, tc.wantCalls)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("unmountShmForCRIU() error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

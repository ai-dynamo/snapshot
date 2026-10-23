// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/require"

	"github.com/ai-dynamo/snapshot/agent/internal/cuda"
	"github.com/ai-dynamo/snapshot/api/compat"
)

const (
	wholeGPUListing = `printf '%s\n' 'GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-5c89852c-d268-c3f3-1b07-005d5ae1dc3f)'`
	migSliceListing = wholeGPUListing + ` '  MIG 1g.5gb      Device  0: (UUID: MIG-a)'`
)

// installFakeMIGListing answers the nvidia-smi -L probe for pid 42 with body,
// and fails it when called any other way.
func installFakeMIGListing(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nset -eu\n" +
		`test "$*" = "--mount=/host/proc/42/ns/mnt --pid=/host/proc/42/ns/pid -- nvidia-smi -L"` + "\n" +
		body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "nsenter"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake nsenter: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRefuseMIGCapture(t *testing.T) {
	tests := []struct {
		name    string
		listing string
		want    string
	}{
		{
			name:    "a slice is refused",
			listing: migSliceListing + ` '  MIG 1g.5gb      Device  1: (UUID: MIG-b)'`,
			want:    "refused as incompatible: mig-slice: the container holds MIG slice 1g.5gb MIG-a, 1g.5gb MIG-b, and Snapshot supports whole GPUs only",
		},
		{name: "a whole GPU is captured", listing: wholeGPUListing},
		{name: "slices that cannot be listed are captured", listing: "exit 17"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			installFakeMIGListing(t, tc.listing)
			err := refuseMIGCapture(logr.Discard(), 42, cuda.ListVisibleGPUs(context.Background(), "/host/proc", 42))
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, tc.want)
		})
	}
}

func TestRefuseMIGRestore(t *testing.T) {
	tests := []struct {
		name    string
		listing string
		skip    bool
		want    string
	}{
		{
			name:    "a slice is refused at the inspect gate",
			listing: migSliceListing,
			want:    "restore refused as incompatible: mig-slice: source whole GPU, target MIG slice 1g.5gb MIG-a",
		},
		{name: "a whole GPU is admitted", listing: wholeGPUListing},
		{name: "slices that cannot be listed are admitted", listing: "exit 17"},
		{name: "a skipped check admits a slice", listing: migSliceListing, skip: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			installFakeMIGListing(t, tc.listing)
			err := refuseMIGRestore(testr.New(t), 42, cuda.ListVisibleGPUs(context.Background(), "/host/proc", 42), tc.skip)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refuseMIGRestore() = %v, want nil", err)
				}
				return
			}
			var incompatible *compat.IncompatibleError
			if !errors.As(err, &incompatible) || incompatible.Gate != compat.GateInspect {
				t.Fatalf("refuseMIGRestore() = %v, want an inspect gate refusal", err)
			}
			if err.Error() != tc.want {
				t.Fatalf("refuseMIGRestore() = %q, want %q", err, tc.want)
			}
		})
	}
}

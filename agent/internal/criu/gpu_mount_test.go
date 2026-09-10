// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"os"
	"testing"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

func TestConfigureGPUExternalMounts(t *testing.T) {
	t.Setenv(additionalMountsEnv, "src=/dev/old,dst=/dev/old")
	m := &types.CheckpointManifest{
		CRIUDump: types.CRIUDumpManifest{ExtMnt: map[string]string{
			"gpu": "/dev/nvidia7",
			"uvm": "/dev/nvidia-uvm",
			"gsp": "/usr/lib/firmware/nvidia/615.45/gsp_tu10x.bin",
		}},
	}

	reset, err := ConfigureGPUExternalMounts(m)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := os.Getenv(additionalMountsEnv),
		"src=/dev/old,dst=/dev/old;src=/dev/nvidia7,dst=/dev/nvidia7;src=/usr/lib/firmware/nvidia/615.45/gsp_tu10x.bin,dst=/usr/lib/firmware/nvidia/615.45/gsp_tu10x.bin"; got != want {
		t.Fatalf("%s = %q, want %q", additionalMountsEnv, got, want)
	}
	reset()
	if got, want := os.Getenv(additionalMountsEnv), "src=/dev/old,dst=/dev/old"; got != want {
		t.Fatalf("reset %s = %q, want %q", additionalMountsEnv, got, want)
	}
}

func TestConfigureGPUExternalMountsIgnoresOtherMounts(t *testing.T) {
	t.Setenv(additionalMountsEnv, "")
	m := &types.CheckpointManifest{
		CRIUDump: types.CRIUDumpManifest{ExtMnt: map[string]string{
			"ctl":  "/dev/nvidiactl",
			"root": "/",
		}},
	}

	reset, err := ConfigureGPUExternalMounts(m)
	if err != nil {
		t.Fatal(err)
	}
	defer reset()
	if got := os.Getenv(additionalMountsEnv); got != "" {
		t.Fatalf("%s = %q, want empty", additionalMountsEnv, got)
	}
}

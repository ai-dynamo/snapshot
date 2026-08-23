// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package compat

import (
	"testing"
)

func populatedFacts() Facts {
	return Facts{
		KernelVersion: "6.8.0-45-generic",
		CPUArch:       "amd64",
		Image:         "nvcr.io/nvidia/tritonserver:24.09",
		ImageID:       "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		CPULimit:      "8",
		MemoryLimit:   "32Gi",
		DriverVersion: "580.82.07",
		GPUDevices: []GPUDevice{
			{UUID: "GPU-1111", ProductName: "Tesla T4"},
		},
		ExternalizedMounts: []string{"/model-cache"},
		ExistingMounts:     []string{"/model-cache"},
	}
}

func differentFacts() Facts {
	return Facts{
		KernelVersion: "5.15.0-89-generic",
		CPUArch:       "arm64",
		Image:         "nvcr.io/nvidia/tritonserver:24.01",
		ImageID:       "sha256:2222222222222222222222222222222222222222222222222222222222222222",
		CPULimit:      "1",
		MemoryLimit:   "1Gi",
		DriverVersion: "560.35.03",
		GPUDevices: []GPUDevice{
			{UUID: "GPU-2222", ProductName: "NVIDIA A100-SXM4-40GB"},
		},
		ExternalizedMounts: []string{"/model-cache"},
		ExistingMounts:     nil,
	}
}

// withChecks swaps the policy table for the length of one test, so what the
// table itself does can be pinned with a rule made up here rather than with
// whichever real rules happen to be registered.
func withChecks(t *testing.T, checks ...check) {
	t.Helper()
	saved := checksByGate
	checksByGate = registerChecks(checks...)
	t.Cleanup(func() { checksByGate = saved })
}

// A rule runs at its own gate and at no other, and what it reports comes back
// named after it. The two gates read different facts, so a rule that ran at the
// wrong one would compare against facts nobody had gathered yet.
func TestCompareRunsTheRulesOfOneGate(t *testing.T) {
	archCheck := check{
		name: "fixture",
		gate: GatePreflight,
		compare: func(source, target Facts) []Mismatch {
			return []Mismatch{{Source: source.CPUArch, Target: target.CPUArch}}
		},
	}

	t.Run("at its own gate", func(t *testing.T) {
		withChecks(t, archCheck)

		want := Mismatch{Check: "fixture", Source: "amd64", Target: "arm64"}
		got := Compare(GatePreflight, populatedFacts(), differentFacts())
		if len(got) != 1 || got[0] != want {
			t.Fatalf("Compare at the preflight gate = %v, want exactly %v", got, want)
		}
	})

	t.Run("and nowhere else", func(t *testing.T) {
		withChecks(t, archCheck)

		if got := Compare(GateInspect, populatedFacts(), differentFacts()); len(got) != 0 {
			t.Fatalf("Compare at the inspect gate = %v, want no mismatches", got)
		}
	})

	t.Run("a table with no rules refuses nothing", func(t *testing.T) {
		withChecks(t)

		for _, gate := range []Gate{GatePreflight, GateInspect} {
			if got := Compare(gate, populatedFacts(), differentFacts()); len(got) != 0 {
				t.Fatalf("Compare at the %q gate = %v, want no mismatches", gate, got)
			}
		}
	})
}

// A rule pinned to a gate nothing calls would never run, and nothing would say
// so. Registration refuses to build such a table at all.
func TestRegisterChecksRejectsAGateNothingCalls(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("registerChecks built a table holding a rule at a gate nothing calls")
		}
	}()

	registerChecks(check{name: "fixture", gate: "nowhere"})
}

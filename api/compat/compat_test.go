// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package compat

import (
	"errors"
	"fmt"
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

// deliberatelyNotSilent holds the rules that do refuse on a fact the target side
// does not carry, for the reasons given where each is defined: the mount rule is
// handed a target list resolved from the source list, and the GPU count is only
// ever compared after discovery has run. An absence in either is a thing looked
// for and not found rather than a thing nobody read.
var deliberatelyNotSilent = map[Check]bool{
	CheckMount:    true,
	CheckGPUCount: true,
}

// Whatever rules are registered, a fact nobody recorded cannot refuse anything:
// every checkpoint captured before a fact existed has to stay restorable, and a
// target the agent could not read has to be given the benefit of the doubt.
func TestCompareIgnoresUnknownFacts(t *testing.T) {
	tests := []struct {
		name   string
		source Facts
		target Facts
	}{
		{
			name: "neither side knows anything",
		},
		{
			name:   "the checkpoint recorded facts the target cannot describe",
			source: populatedFacts(),
		},
		{
			name:   "the target describes facts the checkpoint never recorded",
			target: populatedFacts(),
		},
		{
			name:   "both sides agree",
			source: populatedFacts(),
			target: populatedFacts(),
		},
	}

	for _, gate := range []Gate{GatePreflight, GateInspect} {
		for _, tc := range tests {
			t.Run(string(gate)+" "+tc.name, func(t *testing.T) {
				for _, mismatch := range Compare(gate, tc.source, tc.target) {
					if deliberatelyNotSilent[mismatch.Check] {
						continue
					}
					t.Errorf("Compare(%q) reported %+v, want no mismatches", gate, mismatch)
				}
			})
		}
	}
}

// Every registered rule reports itself, or a refusal names an empty check and
// nobody can tell which rule turned the restore down. Registration has already
// rejected a rule at a gate nothing calls, so the name is what is left to check.
func TestEveryCheckIsNamedAndRegisteredOnce(t *testing.T) {
	seen := map[Check]bool{}
	for gate, registered := range checksByGate {
		for _, c := range registered {
			if c.name == "" {
				t.Errorf("the %q gate holds a rule with no name", gate)
			}
			if seen[c.name] {
				t.Errorf("check %q is registered twice", c.name)
			}
			seen[c.name] = true
		}
	}
}

// Compare has to attribute a mismatch to the rule that found it, since the whole
// refusal vocabulary is built on the check name.
func TestCompareNamesTheFailingCheck(t *testing.T) {
	mismatches := Compare(GatePreflight, populatedFacts(), differentFacts())

	if len(mismatches) == 0 {
		t.Fatal("Compare found nothing wrong between two entirely different machines")
	}
	for _, mismatch := range mismatches {
		if mismatch.Check == "" {
			t.Errorf("mismatch %+v does not name the check that reported it", mismatch)
		}
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

// A refusal has to survive the trip back to the caller that reports it: it
// crosses the restore call chain and is wrapped on the way, and the reader still
// has to tell it apart from a CRIU failure and learn which gate produced it.
func TestNewIncompatibleError(t *testing.T) {
	mismatches := []Mismatch{
		{Check: "cpu-arch", Source: "amd64", Target: "arm64"},
		{Check: "memory-limit", Source: "32Gi", Target: "1Gi"},
	}
	err := NewIncompatibleError(GateInspect, mismatches)

	var incompatible *IncompatibleError
	if !errors.As(fmt.Errorf("restore worker: %w", err), &incompatible) {
		t.Fatal("wrapped incompatible error did not unwrap to *IncompatibleError")
	}
	if incompatible.Gate != GateInspect {
		t.Fatalf("gate = %q, want %q", incompatible.Gate, GateInspect)
	}
	want := "restore refused as incompatible: cpu-arch: source amd64, target arm64; memory-limit: source 32Gi, target 1Gi"
	if got := err.Error(); got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}

	// The caller's slice keeps changing after the refusal is built, and the
	// refusal is what gets reported.
	mismatches[0].Target = "mutated"
	if incompatible.Mismatches[0].Target != "arm64" {
		t.Fatalf("error kept a reference to the caller's slice: %#v", incompatible.Mismatches)
	}
}

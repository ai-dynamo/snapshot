// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package compat compares what a checkpoint was captured on against what a
// restore target offers, so an incompatible restore is refused up front instead
// of failing deep inside CRIU with an unattributable error.
//
// The node agent is the only consumer today. It sits in the api module because
// the check names are protocol, published verbatim on pod conditions and events,
// and so the operator can surface the same recorded facts on the content status
// without a second vocabulary growing up beside this one.
package compat

import "fmt"

// Gate names the moment a comparison runs. The two gates see different facts:
// only the later one can read the node's GPUs and the target's rootfs.
type Gate string

const (
	// GatePreflight runs before the agent claims a restore attempt, where the
	// checkpoint manifest and the target pod are all that is readable.
	GatePreflight Gate = "preflight"

	// GateInspect runs once the placeholder container is resolved, where the
	// GPUs it sees and the mounts under its rootfs become readable.
	GateInspect Gate = "inspect"
)

// Check identifies one comparison rule. It is reported verbatim to users and to
// tooling that branches on it, so a name never changes once released.
type Check string

// Facts is one side of a comparison: the machine, pod, GPU and mount state a
// checkpoint was captured on, or the state a restore target offers.
//
// Every field is optional. A fact missing on either side is unknown rather than
// mismatched, because a checkpoint captured before that fact was ever recorded
// has to stay restorable. A producer that reads only some of these returns a
// Facts with those set and leaves its caller to fill in the rest.
type Facts struct {
	KernelVersion string
	CPUArch       string

	Image       string
	ImageID     string
	CPULimit    string
	MemoryLimit string

	DriverVersion string
	GPUDevices    []GPUDevice

	// ExternalizedMounts holds the mount destinations CRIU externalized at
	// capture.
	ExternalizedMounts []string

	// ExistingMounts holds the destinations that resolve on this machine. The
	// agent resolves them before comparing, so a comparison never touches disk.
	ExistingMounts []string
}

// GPUFacts is what discovery reads off a node before it is folded into a Facts.
// Discovery has no business carrying kernel versions and image digests around,
// so it keeps a type of its own.
type GPUFacts struct {
	DriverVersion string
	Devices       []GPUDevice
}

// GPUDevice is one visible GPU.
type GPUDevice struct {
	UUID        string
	ProductName string
}

// Mismatch is one rule the target failed, carrying both compared values so the
// reported reason can name them.
type Mismatch struct {
	Check  Check
	Source string
	Target string
}

// IncompatibleError reports a restore the target cannot run. It is terminal and
// distinct from every other restore error: no CRIU work was attempted, and
// retrying on this node cannot change the answer. Both gates raise it, so the
// caller reports one refusal whichever gate turned the restore down.
type IncompatibleError struct {
	Gate       Gate
	Mismatches []Mismatch
}

func NewIncompatibleError(gate Gate, mismatches []Mismatch) *IncompatibleError {
	return &IncompatibleError{Gate: gate, Mismatches: append([]Mismatch(nil), mismatches...)}
}

func (e *IncompatibleError) Error() string {
	return "restore refused as incompatible: " + Reasons(e.Mismatches)
}

// check is one row of the policy table. compare returns nil when the rule passes
// or when a fact it needs is unknown, and may report more than one mismatch when
// a rule covers several values.
type check struct {
	name    Check
	gate    Gate
	compare func(source, target Facts) []Mismatch
}

// checksByGate is the policy table: every compatibility rule, partitioned by the
// gate that can evaluate it and kept in the order they are reported.
var checksByGate = registerChecks(
	cpuArchCheck,
	kernelVersionCheck,
	kernelMinimumCheck,
	imageDigestCheck,
	memoryLimitCheck,
)

// registerChecks partitions the policy table by the gate each rule runs at, so
// a comparison indexes its rules instead of walking past the ones belonging to
// the other gate. A rule pinned to a gate nothing calls would never run and
// nothing would say so, so the table refuses to be built at all.
func registerChecks(checks ...check) map[Gate][]check {
	byGate := make(map[Gate][]check)
	for _, c := range checks {
		switch c.gate {
		case GatePreflight, GateInspect:
			byGate[c.gate] = append(byGate[c.gate], c)
		default:
			panic(fmt.Sprintf("compat: check %q runs at gate %q, which nothing calls", c.name, c.gate))
		}
	}
	return byGate
}

// Compare reports every rule the target fails at the given gate. An empty result
// means the restore may proceed.
func Compare(gate Gate, source, target Facts) []Mismatch {
	var mismatches []Mismatch
	for _, c := range checksByGate[gate] {
		for _, mismatch := range c.compare(source, target) {
			mismatch.Check = c.name
			mismatches = append(mismatches, mismatch)
		}
	}
	return mismatches
}

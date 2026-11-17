// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"math/rand"
	"reflect"
	"slices"
	"testing"
)

func observedNode(id int) *int { return &id }

func exampleRequest() Request {
	return Request{
		Sources: []SourceWorker{{
			ID: "worker-b", GPU: "source-gpu", GPUHostNode: observedNode(0),
			AffinityNode: observedNode(0), MemoryPolicyNode: observedNode(0), MemoryPolicy: "bind", MinimumCPUs: 2,
		}},
		Targets: []TargetWorker{{
			ID: "worker-b", SourceGPU: "source-gpu", GPU: "dest-gpu", AllowedCPUs: []int{9, 4, 2, 3},
			AllowedMemoryNodes: []int{30, 50},
		}},
		CUDAAssignments: []GPUAssignment{{SourceGPU: "source-gpu", TargetGPU: "dest-gpu"}},
		Nodes:           []Node{{ID: 20, CPUs: []int{4, 3}}, {ID: 30, CPUs: []int{}}, {ID: 50, CPUs: []int{2, 9}}},
		GPUs:            []GPU{{ID: "dest-gpu", CPUNode: observedNode(20), MemoryNode: observedNode(30)}},
	}
}

func TestKnownAnswerUsesDestinationNotSourceIDs(t *testing.T) {
	r := Resolve(exampleRequest())
	want := []Candidate{{Worker: "worker-b", GPU: "dest-gpu", CPUNode: 20, MemoryNode: 30, CPUs: []int{3, 4}}}
	if r.Status != Feasible || !reflect.DeepEqual(r.Candidates, want) {
		t.Fatalf("got %+v, want candidates %+v", r, want)
	}
}

func TestRestrictedAllowedMasks(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Request)
		want Status
	}{
		{"CPU count", func(r *Request) { r.Targets[0].AllowedCPUs = []int{3} }, Infeasible},
		{"memory nodes", func(r *Request) { r.Targets[0].AllowedMemoryNodes = []int{50} }, Infeasible},
		{"observed empty CPUs", func(r *Request) { r.Targets[0].AllowedCPUs = []int{} }, Infeasible},
		{"observed empty mems", func(r *Request) { r.Targets[0].AllowedMemoryNodes = []int{} }, Infeasible},
		{"unobserved CPUs", func(r *Request) { r.Targets[0].AllowedCPUs = nil }, Unknown},
		{"unobserved mems", func(r *Request) { r.Targets[0].AllowedMemoryNodes = nil }, Unknown},
		{"unobserved node CPUs", func(r *Request) { r.Nodes[0].CPUs = nil }, Unknown},
		{"unknown GPU CPU node", func(r *Request) { r.GPUs[0].CPUNode = observedNode(UnknownNode) }, Unknown},
		{"unknown GPU memory node", func(r *Request) { r.GPUs[0].MemoryNode = observedNode(UnknownNode) }, Unknown},
		{"missing GPU from topology", func(r *Request) { r.GPUs[0].ID = "other" }, Unknown},
		{"missing destination assignment", func(r *Request) { r.Targets = nil }, Unknown},
		{"missing source GPU mapping", func(r *Request) { r.Targets[0].SourceGPU = "" }, Unknown},
		{"source GPU mapping mismatch", func(r *Request) { r.Targets[0].SourceGPU = "wrong-source" }, Unsupported},
		{"missing node from topology", func(r *Request) { r.GPUs[0].CPUNode = observedNode(51) }, Unknown},
		{"invalid negative GPU node", func(r *Request) { r.GPUs[0].CPUNode = observedNode(-2) }, Unsupported},
		{"duplicated worker CPU", func(r *Request) { r.Targets[0].AllowedCPUs = []int{3, 3} }, Unsupported},
		{"duplicated allowed memory node", func(r *Request) { r.Targets[0].AllowedMemoryNodes = []int{30, 30} }, Unsupported},
		{"overlapping NUMA CPU sets", func(r *Request) { r.Nodes[1].CPUs = []int{3} }, Unsupported},
		{"duplicated NUMA node", func(r *Request) { r.Nodes[1].ID = 20 }, Unsupported},
		{"duplicated GPU", func(r *Request) { r.GPUs = append(r.GPUs, r.GPUs[0]) }, Unsupported},
		{"no sources", func(r *Request) { r.Sources = nil }, Unsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := exampleRequest()
			tc.edit(&r)
			got := Resolve(r)
			if got.Status != tc.want || got.Candidates != nil {
				t.Fatalf("got %+v, expected %s refusal without partial candidates", got, tc.want)
			}
		})
	}
}

func TestUnsupportedSourcePlacementCannotBeSilentlyTranslated(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Request)
		want Status
	}{
		{"source affinity remote", func(r *Request) { r.Sources[0].AffinityNode = observedNode(5) }, Unsupported},
		{"source memory remote", func(r *Request) { r.Sources[0].MemoryPolicyNode = observedNode(5) }, Unsupported},
		{"source policy interleave", func(r *Request) { r.Sources[0].MemoryPolicy = "interleave" }, Unsupported},
		{"source policy unobserved", func(r *Request) { r.Sources[0].MemoryPolicy = "" }, Unknown},
		{"source GPU node unobserved", func(r *Request) { r.Sources[0].GPUHostNode = observedNode(UnknownNode) }, Unknown},
		{"source affinity unobserved", func(r *Request) { r.Sources[0].AffinityNode = observedNode(UnknownNode) }, Unknown},
		{"source memory node unobserved", func(r *Request) { r.Sources[0].MemoryPolicyNode = observedNode(UnknownNode) }, Unknown},
		{"minimum CPU count zero", func(r *Request) { r.Sources[0].MinimumCPUs = 0 }, Unsupported},
		{"missing source GPU", func(r *Request) { r.Sources[0].GPU = "" }, Unsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := exampleRequest()
			tc.edit(&r)
			if got := Resolve(r); got.Status != tc.want {
				t.Fatalf("got %+v, want %s", got, tc.want)
			}
		})
	}
}

func TestMultipleWorkersAndNoPartialPlan(t *testing.T) {
	r := exampleRequest()
	second := r.Sources[0]
	second.ID = "worker-a"
	r.Sources = append(r.Sources, second)
	other := r.Targets[0]
	other.ID = "worker-a"
	r.Targets = append(r.Targets, other)

	got := Resolve(r)
	if got.Status != Feasible || len(got.Candidates) != 2 || got.Candidates[0].Worker != "worker-a" || got.Candidates[1].Worker != "worker-b" {
		t.Fatalf("unexpected complete plan: %+v", got)
	}
	// CPU affinity is not exclusive; workers may share permitted CPUs.
	if !reflect.DeepEqual(got.Candidates[0].CPUs, got.Candidates[1].CPUs) {
		t.Fatalf("shared allowed CPUs rejected: %+v", got)
	}

	r.Targets[0].AllowedMemoryNodes = []int{50}
	got = Resolve(r)
	if got.Status != Infeasible || got.Candidates != nil {
		t.Fatalf("a failing worker must reject the entire plan: %+v", got)
	}
}

func TestInputOrderAndAliasing(t *testing.T) {
	r := exampleRequest()
	a := Resolve(r)
	r.Nodes[0], r.Nodes[1] = r.Nodes[1], r.Nodes[0]
	r.GPUs = append(r.GPUs, GPU{ID: "unused", CPUNode: observedNode(50), MemoryNode: observedNode(30)})
	r.GPUs[0], r.GPUs[1] = r.GPUs[1], r.GPUs[0]
	if b := Resolve(r); !reflect.DeepEqual(a, b) {
		t.Fatalf("reordering unrelated topology changed the plan: %+v vs %+v", a, b)
	}

	// A returned candidate must not alias a caller-owned CPU slice.
	r = exampleRequest()
	plan := Resolve(r)
	r.Targets[0].AllowedCPUs[1] = 999
	r.Nodes[0].CPUs[0] = 888
	if !reflect.DeepEqual(plan.Candidates[0].CPUs, []int{3, 4}) {
		t.Fatalf("returned plan aliases observed input: %+v", plan)
	}
}

func TestNUMALabelTransport(t *testing.T) {
	rng := rand.New(rand.NewSource(548))
	for trial := 0; trial < 150; trial++ {
		req := exampleRequest()
		orig := Resolve(req)
		perm := rng.Perm(3)
		mapping := map[int]int{20: perm[0]*13 + 100, 30: perm[1]*13 + 100, 50: perm[2]*13 + 100}
		for i := range req.Nodes {
			req.Nodes[i].ID = mapping[req.Nodes[i].ID]
		}
		for i := range req.GPUs {
			req.GPUs[i].CPUNode = observedNode(mapping[*req.GPUs[i].CPUNode])
			req.GPUs[i].MemoryNode = observedNode(mapping[*req.GPUs[i].MemoryNode])
		}
		for i, node := range req.Targets[0].AllowedMemoryNodes {
			req.Targets[0].AllowedMemoryNodes[i] = mapping[node]
		}
		newPlan := Resolve(req)
		if newPlan.Status != Feasible || !slices.Equal(newPlan.Candidates[0].CPUs, orig.Candidates[0].CPUs) || newPlan.Candidates[0].CPUNode != mapping[20] || newPlan.Candidates[0].MemoryNode != mapping[30] {
			t.Fatalf("trial %d: relabel broke locality: %+v", trial, newPlan)
		}
	}
}

func TestCPUIdentifierTransport(t *testing.T) {
	req := exampleRequest()
	for i := range req.Nodes {
		for j := range req.Nodes[i].CPUs {
			req.Nodes[i].CPUs[j] += 100
		}
	}
	for i := range req.Targets[0].AllowedCPUs {
		req.Targets[0].AllowedCPUs[i] += 100
	}
	result := Resolve(req)
	if result.Status != Feasible || !slices.Equal(result.Candidates[0].CPUs, []int{103, 104}) {
		t.Fatalf("CPU ID relabeling must be carried through the plan: %+v", result)
	}
}

func TestIndependentReferenceMaskIntersection(t *testing.T) {
	// Distinct test oracle: enumerate the allowed mask and check membership in
	// the node's CPU set by linear search, instead of using the planner's map.
	rng := rand.New(rand.NewSource(881))
	for k := 0; k < 250; k++ {
		r := exampleRequest()
		allowed := make([]int, 0, 6)
		for cpu := 1; cpu <= 6; cpu++ {
			if rng.Intn(2) == 1 {
				allowed = append(allowed, cpu)
			}
		}
		r.Targets[0].AllowedCPUs = allowed
		r.Sources[0].MinimumCPUs = 1 + rng.Intn(3)
		wantCPU := make([]int, 0)
		for _, cpu := range allowed {
			if slices.Contains(r.Nodes[0].CPUs, cpu) {
				wantCPU = append(wantCPU, cpu)
			}
		}
		slices.Sort(wantCPU)
		got := Resolve(r)
		if len(wantCPU) < r.Sources[0].MinimumCPUs {
			if got.Status != Infeasible || got.Candidates != nil {
				t.Fatalf("trial %d: expected insufficient CPUs, got %+v", k, got)
			}
		} else if got.Status != Feasible || !slices.Equal(got.Candidates[0].CPUs, wantCPU) {
			t.Fatalf("trial %d: got %+v, expected %+v", k, got, wantCPU)
		}
	}
}

// Zero-value integers are not observations. The old contract accepted a
// completely omitted source/GPU node description as observed NUMA 0.
func TestUnobservedZeroValueCannotAuthorizePlacement(t *testing.T) {
	base := exampleRequest()
	base.Nodes = []Node{{ID: 0, CPUs: []int{2, 3}}}
	base.Targets[0].AllowedCPUs = []int{2, 3}
	base.Targets[0].AllowedMemoryNodes = []int{0}
	base.GPUs[0] = GPU{ID: "dest-gpu", CPUNode: observedNode(0), MemoryNode: observedNode(0)}
	base.Sources[0] = SourceWorker{
		ID: "worker-b", GPU: "source-gpu", GPUHostNode: observedNode(0),
		AffinityNode: observedNode(0), MemoryPolicyNode: observedNode(0),
		MemoryPolicy: "bind", MinimumCPUs: 1,
	}
	if got := Resolve(base); got.Status != Feasible {
		t.Fatalf("real observed NUMA 0 must remain feasible: %+v", got)
	}
	cases := []struct {
		name string
		edit func(*Request)
	}{
		{"missing all source nodes", func(r *Request) {
			r.Sources[0] = SourceWorker{ID: "worker-b", GPU: "source-gpu", MemoryPolicy: "bind", MinimumCPUs: 1}
		}},
		{"missing source GPU node", func(r *Request) { r.Sources[0].GPUHostNode = nil }},
		{"missing source affinity node", func(r *Request) { r.Sources[0].AffinityNode = nil }},
		{"missing source memory node", func(r *Request) { r.Sources[0].MemoryPolicyNode = nil }},
		{"missing GPU CPU proximity", func(r *Request) { r.GPUs[0].CPUNode = nil }},
		{"missing GPU memory proximity", func(r *Request) { r.GPUs[0].MemoryNode = nil }},
		{"GPU with omitted zero-valued nodes", func(r *Request) { r.GPUs[0] = GPU{ID: "dest-gpu"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			r.Sources = slices.Clone(base.Sources)
			r.GPUs = slices.Clone(base.GPUs)
			tc.edit(&r)
			got := Resolve(r)
			if got.Status != Unknown || got.Candidates != nil {
				t.Fatalf("unobserved placement accepted: %+v", got)
			}
		})
	}
}

// An injected many-to-one or one-to-many GPU map can satisfy per-worker
// locality checks even though it violates the one-to-one GPU assignment
// contract needed for ordinary (non-MIG) restore. A shared source GPU across
// workers is supported, but it must map consistently to one destination GPU.
func TestDistinctSourceGPUAssignmentsMustRemainInjective(t *testing.T) {
	base := exampleRequest()
	second := base.Sources[0]
	second.ID = "worker-a"
	second.GPU = "source-gpu-2"
	base.Sources = append(base.Sources, second)
	other := base.Targets[0]
	other.ID = second.ID
	other.SourceGPU = second.GPU
	// Deliberately leave other.GPU = dest-gpu, collapsing two sources.
	base.Targets = append(base.Targets, other)
	base.CUDAAssignments = append(base.CUDAAssignments, GPUAssignment{SourceGPU: "source-gpu-2", TargetGPU: "dest-gpu"})
	if got := Resolve(base); got.Status != Unsupported || got.Candidates != nil {
		t.Fatalf("two source GPUs collapsed onto one destination GPU: %+v", got)
	}

	// Distinct GPU assignments remain feasible when each source is assigned
	// a different destination GPU, even if both are near the same NUMA nodes.
	base.GPUs = append(base.GPUs, GPU{ID: "dest-gpu-2", CPUNode: observedNode(20), MemoryNode: observedNode(30)})
	base.Targets[1].GPU = "dest-gpu-2"
	base.CUDAAssignments[1].TargetGPU = "dest-gpu-2"
	if got := Resolve(base); got.Status != Feasible || len(got.Candidates) != 2 {
		t.Fatalf("valid distinct GPU assignments rejected: %+v", got)
	}
	base.Targets[1].GPU = "dest-gpu"

	// Two workers on the *same* source GPU may legitimately share one
	// destination GPU, as long as their allowed masks are sufficient.
	base.Sources[1].GPU = base.Sources[0].GPU
	base.Targets[1].SourceGPU = base.Sources[0].GPU
	base.CUDAAssignments = base.CUDAAssignments[:1]
	if got := Resolve(base); got.Status != Feasible || len(got.Candidates) != 2 {
		t.Fatalf("shared source GPU should admit a consistent map: %+v", got)
	}

	// The same source GPU must not fan out to two different destination GPUs.
	base.Targets[1].GPU = "dest-gpu-2"
	if got := Resolve(base); got.Status != Unsupported || got.Candidates != nil {
		t.Fatalf("one source GPU mapped to two destination GPUs: %+v", got)
	}
}

// A worker's claimed GPU assignment must agree with the independently
// supplied CUDA device-map relation. A consistent one-to-one worker map alone
// is insufficient, as the red/green regression demonstrates.
func TestCUDADeviceMapIsAnIndependentRequiredInput(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Request)
		want Status
	}{
		{"missing CUDA map", func(r *Request) { r.CUDAAssignments = nil }, Unknown},
		{"explicit empty CUDA map", func(r *Request) { r.CUDAAssignments = []GPUAssignment{} }, Unsupported},
		{"unknown source GPU identity", func(r *Request) { r.CUDAAssignments[0].SourceGPU = "" }, Unknown},
		{"unknown target GPU identity", func(r *Request) { r.CUDAAssignments[0].TargetGPU = "" }, Unknown},
		{"CUDA map missing worker source GPU", func(r *Request) { r.CUDAAssignments[0].SourceGPU = "other-source" }, Unknown},
		{"wrong GPU mapping on known topology", func(r *Request) {
			r.GPUs = append(r.GPUs, GPU{ID: "other-target", CPUNode: observedNode(20), MemoryNode: observedNode(30)})
			r.CUDAAssignments[0].TargetGPU = "other-target"
		}, Unsupported},
		{"duplicate source GPU in CUDA map", func(r *Request) {
			r.CUDAAssignments = append(r.CUDAAssignments, r.CUDAAssignments[0])
		}, Unsupported},
		{"distinct source GPU mapped to same target GPU", func(r *Request) {
			r.CUDAAssignments = append(r.CUDAAssignments, GPUAssignment{SourceGPU: "another-source", TargetGPU: "dest-gpu"})
		}, Unsupported},
		{"CUDA map pointing to omitted destination GPU", func(r *Request) {
			r.CUDAAssignments[0].TargetGPU = "unobserved-target"
		}, Unknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := exampleRequest()
			tc.edit(&r)
			got := Resolve(r)
			if got.Status != tc.want || got.Candidates != nil {
				t.Fatalf("got %+v; want %q with no candidates", got, tc.want)
			}
		})
	}
}

// Source and destination sets can have the same UUIDs in different discovery
// orders. Snapshot's BuildDeviceMap returns the empty helper map in that case,
// which denotes UUID identity rather than positional remapping.
func TestCUDAIdentityMappingWhenDiscoveryOrderChanges(t *testing.T) {
	r := exampleRequest()
	source2 := r.Sources[0]
	source2.ID, source2.GPU = "worker-z", "GPU-B"
	r.Sources[0].GPU = "GPU-A"
	r.Sources = append(r.Sources, source2)
	r.Targets[0].SourceGPU, r.Targets[0].GPU = "GPU-A", "GPU-A"
	target2 := r.Targets[0]
	target2.ID, target2.SourceGPU, target2.GPU = "worker-z", "GPU-B", "GPU-B"
	r.Targets = append(r.Targets, target2)
	// Even when discovery order is [GPU-B,GPU-A], both UUIDs map to themselves.
	r.CUDAAssignments = []GPUAssignment{{SourceGPU: "GPU-B", TargetGPU: "GPU-B"}, {SourceGPU: "GPU-A", TargetGPU: "GPU-A"}}
	r.GPUs = []GPU{
		{ID: "GPU-B", CPUNode: observedNode(20), MemoryNode: observedNode(30)},
		{ID: "GPU-A", CPUNode: observedNode(20), MemoryNode: observedNode(30)},
	}
	got := Resolve(r)
	if got.Status != Feasible || len(got.Candidates) != 2 {
		t.Fatalf("identity GPU mapping rejected: %+v", got)
	}
	r.Targets[0].GPU, r.Targets[1].GPU = "GPU-B", "GPU-A"
	got = Resolve(r)
	if got.Status != Unsupported || got.Candidates != nil {
		t.Fatalf("swapped map accepted despite UUID identity: %+v", got)
	}
}

// With distinct source and destination sets, Snapshot's BuildDeviceMap pairs
// the GPU UUIDs in bus/ordinal order, even if some UUIDs occur in both sets.
// Here the canonical mapping is source A -> destination B and source B -> C.
func TestCUDAOverlappingSetsNeedPositionalMapping(t *testing.T) {
	r := exampleRequest()
	r.Sources[0].GPU = "GPU-A"
	r.Targets[0].SourceGPU, r.Targets[0].GPU = "GPU-A", "GPU-B"
	second := r.Sources[0]
	second.ID, second.GPU = "worker-z", "GPU-B"
	r.Sources = append(r.Sources, second)
	secondTarget := r.Targets[0]
	secondTarget.ID, secondTarget.SourceGPU, secondTarget.GPU = "worker-z", "GPU-B", "GPU-C"
	r.Targets = append(r.Targets, secondTarget)
	r.GPUs = []GPU{{ID: "GPU-B", CPUNode: observedNode(20), MemoryNode: observedNode(30)}, {ID: "GPU-C", CPUNode: observedNode(20), MemoryNode: observedNode(30)}}
	r.CUDAAssignments = []GPUAssignment{{SourceGPU: "GPU-A", TargetGPU: "GPU-B"}, {SourceGPU: "GPU-B", TargetGPU: "GPU-C"}}
	if got := Resolve(r); got.Status != Feasible || len(got.Candidates) != 2 {
		t.Fatalf("valid overlap mapping refused: %+v", got)
	}
	// Incorrectly preserving GPU-B identity despite its actual migration to C
	// is a false-positive hazard. The canonical mapping must take precedence.
	r.Targets[1].GPU = "GPU-B"
	if got := Resolve(r); got.Status != Unsupported || got.Candidates != nil {
		t.Fatalf("incorrect overlap mapping accepted: %+v", got)
	}
}

func TestCUDAAssignmentOrderDoesNotChangeResult(t *testing.T) {
	r := exampleRequest()
	r.CUDAAssignments = append(r.CUDAAssignments, GPUAssignment{SourceGPU: "unused-source", TargetGPU: "unused-target"})
	r.GPUs = append(r.GPUs, GPU{ID: "unused-target", CPUNode: observedNode(20), MemoryNode: observedNode(30)})
	before := Resolve(r)
	r.CUDAAssignments[0], r.CUDAAssignments[1] = r.CUDAAssignments[1], r.CUDAAssignments[0]
	after := Resolve(r)
	if !reflect.DeepEqual(before, after) || before.Status != Feasible {
		t.Fatalf("unrelated CUDA mapping order changed plan: %+v vs %+v", before, after)
	}
}

// A complete CUDA map cannot be accepted with an incomplete GPU inventory,
// even if no supplied worker happens to reference the omitted GPU.
func TestCUDADeviceMapRequiresObservedDestinations(t *testing.T) {
	r := exampleRequest()
	r.CUDAAssignments = append(r.CUDAAssignments, GPUAssignment{SourceGPU: "unused-source", TargetGPU: "not-observed"})
	got := Resolve(r)
	if got.Status != Unknown || got.Candidates != nil {
		t.Fatalf("mapped destination GPU missing from topology was accepted: %+v", got)
	}
}

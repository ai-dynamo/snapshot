// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package placement resolves a small, explicitly supported subset of NUMA
// placement requests without inspecting the host or changing process state.
package placement

import (
	"fmt"
	"slices"
	"sort"
)

// Status distinguishes conditional feasibility under supplied inputs from
// missing or unsupported input. Feasible does not certify applied placement.
type Status string

const (
	Feasible    Status = "feasible"
	Infeasible  Status = "infeasible"
	Unknown     Status = "unknown_topology"
	Unsupported Status = "unsupported_topology"
)

// UnknownNode optionally represents unknown NUMA proximity; nil pointers
// also mean a node observation is missing.
const UnknownNode = -1

// SourceWorker is a validated capture-time summary of a single worker whose
// CPU affinity and single-node MPOL_BIND policy are both GPU-local. The capture
// path must verify that summary; this package does not inspect source threads.
type SourceWorker struct {
	ID               string
	GPU              string
	GPUHostNode      *int   // nil means unobserved; a pointer to 0 means NUMA node 0
	AffinityNode     *int   // nil means unobserved
	MemoryPolicyNode *int   // nil means unobserved
	MemoryPolicy     string // only "bind" is supported; empty means not observed
	MinimumCPUs      int    // minimum usable local CPUs, not an exclusive reservation
}

// TargetWorker is the destination GPU assignment and effective permissions
// obtained from external observation. Nil masks mean unknown; empty masks mean
// observed but disallowed. The GPU assignment must come from the restore plan.
type TargetWorker struct {
	ID                 string
	SourceGPU          string // source device identity from the existing CUDA GPU map
	GPU                string
	AllowedCPUs        []int
	AllowedMemoryNodes []int
}

// Node records the CPUs physically associated with an observed host NUMA node.
// Nodes with no CPUs may still be valid host-memory nodes.
type Node struct {
	ID   int
	CPUs []int // nil is unobserved; []int{} is an observed, CPU-less node
}

// GPU records separately observed destination CPU- and memory-near NUMA nodes.
// Nil means the proximity was not observed. A pointer to 0 means NUMA 0.
// UnknownNode is also recognized when present explicitly.
type GPU struct {
	ID         string
	CPUNode    *int
	MemoryNode *int
}

// GPUAssignment binds a source GPU UUID to the destination GPU UUID selected
// by Snapshot's CUDA device mapping. This is an externally supplied contract,
// not a result inferred from a worker's locality or assigned GPU name.
type GPUAssignment struct {
	SourceGPU string
	TargetGPU string
}

// Request supplies the capture intent, CUDA device assignment, destination
// worker association, and observed topology/permissions. CUDAAssignments
// must reflect the actual CUDA mapping, not arbitrary self-consistent pairs.
// A nil CUDAAssignments slice means mapping information is unavailable.
type Request struct {
	Sources         []SourceWorker
	Targets         []TargetWorker
	CUDAAssignments []GPUAssignment
	Nodes           []Node
	GPUs            []GPU
}

// Candidate reports an allowed CPU set and a permitted GPU-near memory node.
// It does not reserve CPU time, set affinity, restore memory policy, or move
// existing pages. Candidates may share CPUs with other workers.
type Candidate struct {
	Worker     string
	GPU        string
	CPUs       []int
	CPUNode    int
	MemoryNode int
}

// Result contains either a complete candidate set or a typed refusal. On
// refusal, Candidates is nil; never consume a partial placement plan.
type Result struct {
	Status     Status
	Reason     string
	Candidates []Candidate
}

func refuse(s Status, format string, args ...any) Result {
	return Result{Status: s, Reason: fmt.Sprintf(format, args...)}
}

func validateIDs(ids []int) error {
	seen := make(map[int]bool, len(ids))
	for _, id := range ids {
		if id < 0 {
			return fmt.Errorf("negative identifier %d", id)
		}
		if seen[id] {
			return fmt.Errorf("duplicate identifier %d", id)
		}
		seen[id] = true
	}
	return nil
}

func localAllowed(nodeCPUs, allowed []int) []int {
	set := make(map[int]bool, len(allowed))
	for _, cpu := range allowed {
		set[cpu] = true
	}
	out := make([]int, 0, len(nodeCPUs))
	for _, cpu := range nodeCPUs {
		if set[cpu] {
			out = append(out, cpu)
		}
	}
	sort.Ints(out)
	return out
}

// Resolve checks strict GPU-local placement for workers whose saved CPU
// affinity and single-node memory policy were GPU-local at capture time.
// It does not handle remote, interleaved or mixed-node source placements.
// It never uses source NUMA identifiers as destination identifiers.
func Resolve(req Request) Result {
	if len(req.Sources) == 0 {
		return refuse(Unsupported, "no source workers supplied")
	}
	if len(req.Nodes) == 0 || len(req.GPUs) == 0 {
		return refuse(Unknown, "destination topology is unavailable")
	}
	if req.CUDAAssignments == nil {
		return refuse(Unknown, "CUDA source-to-destination GPU mapping is unavailable")
	}
	if len(req.CUDAAssignments) == 0 {
		return refuse(Unsupported, "CUDA GPU mapping contains no assignments")
	}
	// Validate the actual device mapping as an independent input. Two workers
	// can share one GPU, but distinct CUDA GPU identities must map bijectively.
	actualCUDA := make(map[string]string, len(req.CUDAAssignments))
	reverseCUDA := make(map[string]string, len(req.CUDAAssignments))
	for _, assignment := range req.CUDAAssignments {
		if assignment.SourceGPU == "" || assignment.TargetGPU == "" {
			return refuse(Unknown, "CUDA GPU mapping contains an unobserved identity")
		}
		if prior, exists := actualCUDA[assignment.SourceGPU]; exists {
			return refuse(Unsupported, "source GPU %q has duplicate CUDA mappings (%q and %q)", assignment.SourceGPU, prior, assignment.TargetGPU)
		}
		if prior, exists := reverseCUDA[assignment.TargetGPU]; exists {
			return refuse(Unsupported, "destination GPU %q maps from source GPUs %q and %q", assignment.TargetGPU, prior, assignment.SourceGPU)
		}
		actualCUDA[assignment.SourceGPU] = assignment.TargetGPU
		reverseCUDA[assignment.TargetGPU] = assignment.SourceGPU
	}

	nodes := make(map[int]Node, len(req.Nodes))
	cpuOwners := make(map[int]int)
	for _, n := range req.Nodes {
		if n.ID < 0 {
			return refuse(Unsupported, "invalid destination NUMA node %d", n.ID)
		}
		if _, exists := nodes[n.ID]; exists {
			return refuse(Unsupported, "duplicate destination NUMA node %d", n.ID)
		}
		if n.CPUs == nil {
			return refuse(Unknown, "CPU membership for destination NUMA node %d is unknown", n.ID)
		}
		if err := validateIDs(n.CPUs); err != nil {
			return refuse(Unsupported, "destination NUMA node %d: %v", n.ID, err)
		}
		for _, cpu := range n.CPUs {
			if owner, ok := cpuOwners[cpu]; ok {
				return refuse(Unsupported, "CPU %d appears in destination NUMA nodes %d and %d", cpu, owner, n.ID)
			}
			cpuOwners[cpu] = n.ID
		}
		nodes[n.ID] = n
	}

	gpus := make(map[string]GPU, len(req.GPUs))
	for _, gpu := range req.GPUs {
		if gpu.ID == "" {
			return refuse(Unsupported, "destination GPU has an empty identity")
		}
		if _, exists := gpus[gpu.ID]; exists {
			return refuse(Unsupported, "duplicate destination GPU %q", gpu.ID)
		}
		if (gpu.CPUNode != nil && *gpu.CPUNode < UnknownNode) || (gpu.MemoryNode != nil && *gpu.MemoryNode < UnknownNode) {
			return refuse(Unsupported, "destination GPU %q has invalid NUMA proximity", gpu.ID)
		}
		gpus[gpu.ID] = gpu
	}
	// Validate every CUDA mapping target, not only GPUs referenced by the
	// supplied workers. Otherwise an incomplete topology can look feasible.
	for _, assignment := range req.CUDAAssignments {
		if _, ok := gpus[assignment.TargetGPU]; !ok {
			return refuse(Unknown, "CUDA-mapped destination GPU %q is not observed", assignment.TargetGPU)
		}
	}

	targets := make(map[string]TargetWorker, len(req.Targets))
	for _, target := range req.Targets {
		if target.ID == "" {
			return refuse(Unsupported, "destination worker has an empty identity")
		}
		if _, exists := targets[target.ID]; exists {
			return refuse(Unsupported, "duplicate destination worker %q", target.ID)
		}
		targets[target.ID] = target
	}
	if len(targets) < len(req.Sources) {
		return refuse(Unknown, "destination worker assignments are incomplete")
	}
	if len(targets) > len(req.Sources) {
		return refuse(Unsupported, "destination worker assignments include unrecognized workers")
	}

	sources := slices.Clone(req.Sources)
	sort.Slice(sources, func(i, j int) bool { return sources[i].ID < sources[j].ID })
	plan := make([]Candidate, 0, len(sources))
	for i, src := range sources {
		if src.ID == "" || src.GPU == "" || (i > 0 && sources[i-1].ID == src.ID) {
			return refuse(Unsupported, "missing or duplicate source worker identity %q", src.ID)
		}
		if src.MinimumCPUs < 1 {
			return refuse(Unsupported, "worker %q needs a positive minimum CPU count", src.ID)
		}
		if src.GPUHostNode == nil || src.AffinityNode == nil || src.MemoryPolicyNode == nil || src.MemoryPolicy == "" {
			return refuse(Unknown, "worker %q source placement is unobserved", src.ID)
		}
		for _, node := range []int{*src.GPUHostNode, *src.AffinityNode, *src.MemoryPolicyNode} {
			if node < UnknownNode {
				return refuse(Unsupported, "worker %q has invalid source NUMA identifier %d", src.ID, node)
			}
			if node == UnknownNode {
				return refuse(Unknown, "worker %q source placement is unobserved", src.ID)
			}
		}
		if src.MemoryPolicy != "bind" || *src.AffinityNode != *src.GPUHostNode || *src.MemoryPolicyNode != *src.GPUHostNode {
			return refuse(Unsupported, "worker %q source placement is outside strict GPU-local bind scope", src.ID)
		}

		dst, ok := targets[src.ID]
		if !ok || dst.GPU == "" || dst.SourceGPU == "" {
			return refuse(Unknown, "worker %q has no complete destination GPU assignment", src.ID)
		}
		if dst.SourceGPU != src.GPU {
			return refuse(Unsupported, "worker %q GPU map originates from %q, expected %q", src.ID, dst.SourceGPU, src.GPU)
		}
		mapped, ok := actualCUDA[src.GPU]
		if !ok {
			return refuse(Unknown, "worker %q source GPU %q has no CUDA device mapping", src.ID, src.GPU)
		}
		if mapped != dst.GPU {
			return refuse(Unsupported, "worker %q assigned destination GPU %q, but CUDA maps source %q to %q", src.ID, dst.GPU, src.GPU, mapped)
		}
		if dst.AllowedCPUs == nil || dst.AllowedMemoryNodes == nil {
			return refuse(Unknown, "worker %q destination CPU/memory permissions are unobserved", src.ID)
		}
		if err := validateIDs(dst.AllowedCPUs); err != nil {
			return refuse(Unsupported, "worker %q destination CPU permissions: %v", src.ID, err)
		}
		if err := validateIDs(dst.AllowedMemoryNodes); err != nil {
			return refuse(Unsupported, "worker %q destination memory permissions: %v", src.ID, err)
		}
		gpu, ok := gpus[dst.GPU]
		if !ok {
			return refuse(Unknown, "worker %q destination GPU %q is not observed", src.ID, dst.GPU)
		}
		if gpu.CPUNode == nil || gpu.MemoryNode == nil || *gpu.CPUNode == UnknownNode || *gpu.MemoryNode == UnknownNode {
			return refuse(Unknown, "GPU %q destination CPU/memory proximity is unknown", gpu.ID)
		}
		cpuNode, ok := nodes[*gpu.CPUNode]
		if !ok {
			return refuse(Unknown, "GPU %q CPU NUMA node %d is not observed", gpu.ID, *gpu.CPUNode)
		}
		if _, ok := nodes[*gpu.MemoryNode]; !ok {
			return refuse(Unknown, "GPU %q memory NUMA node %d is not observed", gpu.ID, *gpu.MemoryNode)
		}
		allowed := localAllowed(cpuNode.CPUs, dst.AllowedCPUs)
		if len(allowed) < src.MinimumCPUs {
			return refuse(Infeasible, "worker %q needs %d GPU-local allowed CPUs, has %d", src.ID, src.MinimumCPUs, len(allowed))
		}
		if !slices.Contains(dst.AllowedMemoryNodes, *gpu.MemoryNode) {
			return refuse(Infeasible, "worker %q cannot allocate on GPU-local memory node %d", src.ID, *gpu.MemoryNode)
		}
		plan = append(plan, Candidate{Worker: src.ID, GPU: gpu.ID, CPUs: allowed, CPUNode: *gpu.CPUNode, MemoryNode: *gpu.MemoryNode})
	}
	return Result{Status: Feasible, Candidates: plan}
}

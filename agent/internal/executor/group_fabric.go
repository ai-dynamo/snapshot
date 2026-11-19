// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"fmt"
	"slices"

	"github.com/ai-dynamo/snapshot/agent/internal/cuda"
	"github.com/ai-dynamo/snapshot/agent/internal/group"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

// readGPUFabricFunc is overridden in tests: cuda.ReadGPUFabric runs
// nvidia-smi against the node's GPUs.
var readGPUFabricFunc = cuda.ReadGPUFabric

// checkFabric refuses a restore that places members whose Pods shared GPU
// memory over NVLink at capture onto GPUs that cannot share it: every GPU of
// those members must be registered with its NVLink fabric, and all of them
// must be in one clique of one cluster. Rank's member checks every member it
// shared memory with, directly or through others, in rank order, so each of
// them reaches the same verdict. A member that shared none checks nothing.
func checkFabric(rank int, admissions []group.Admission) error {
	ranks := sharingGPUMemory(rank, admissions)
	if len(ranks) == 1 {
		return nil
	}
	var first types.GPUFabric
	firstRank := -1
	for _, r := range ranks {
		fabric := admissions[r].Fabric
		if fabric == nil || len(fabric.GPUs) == 0 {
			return fabricRefusal("rank %d reported no NVLink fabric for its GPUs", r)
		}
		for _, gpu := range fabric.GPUs {
			switch {
			case !gpu.Ready():
				return fabricRefusal("rank %d's GPU %s is not ready on its NVLink fabric (state %s, status %s)",
					r, gpu.UUID, gpu.State, gpu.Status)
			case firstRank < 0:
				first, firstRank = gpu, r
			case gpu.ClusterUUID != first.ClusterUUID:
				return fabricRefusal("rank %d's GPU %s is in NVLink cluster %s, but rank %d's GPU %s is in cluster %s",
					r, gpu.UUID, gpu.ClusterUUID, firstRank, first.UUID, first.ClusterUUID)
			case gpu.CliqueID != first.CliqueID:
				return fabricRefusal("rank %d's GPU %s is in NVLink clique %s, but rank %d's GPU %s is in clique %s",
					r, gpu.UUID, gpu.CliqueID, firstRank, first.UUID, first.CliqueID)
			}
		}
	}
	return nil
}

// fabricRefusal is checkFabric's refusal, with what format says differs.
func fabricRefusal(format string, args ...any) error {
	return fmt.Errorf("the members that shared GPU memory over NVLink at checkpoint must be restored onto GPUs "+
		"in one NVLink clique: "+format, args...)
}

// sharingGPUMemory returns rank and the ranks of the members whose Pods shared
// GPU memory over NVLink with rank's, directly or through others, in rank
// order. Each member lists only its own peers, so two members are peers when
// either lists the other.
func sharingGPUMemory(rank int, admissions []group.Admission) []int {
	linked := make([][]int, len(admissions))
	for r, a := range admissions {
		if a.Fabric == nil {
			continue
		}
		for _, peer := range a.Fabric.Peers {
			// Every member checked its peers' ranks before it published them,
			// so this only keeps a broken admission from reaching past the
			// group.
			if peer >= 0 && peer < len(admissions) {
				linked[r] = append(linked[r], peer)
				linked[peer] = append(linked[peer], r)
			}
		}
	}
	seen := make([]bool, len(admissions))
	seen[rank] = true
	ranks := []int{rank}
	for i := 0; i < len(ranks); i++ {
		for _, peer := range linked[ranks[i]] {
			if !seen[peer] {
				seen[peer] = true
				ranks = append(ranks, peer)
			}
		}
	}
	slices.Sort(ranks)
	return ranks
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ai-dynamo/snapshot/agent/internal/group"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

func TestCheckFabric(t *testing.T) {
	const refusal = "the members that shared GPU memory over NVLink at checkpoint must be restored onto GPUs " +
		"in one NVLink clique: "
	notReady := types.GPUFabric{UUID: "GPU-a2", ClusterUUID: "N/A", CliqueID: "N/A", State: "In Progress", Status: "N/A"}
	for _, tc := range []struct {
		name       string
		admissions []group.Admission
		// want is each rank's verdict, by rank: an error, or "" for none.
		want []string
	}{
		{
			name: "the GPUs are in one clique",
			admissions: []group.Admission{
				sharedWith([]int{1}, inClique("GPU-a", "cluster-a", "1"), inClique("GPU-a2", "cluster-a", "1")),
				sharedWith([]int{0}, inClique("GPU-b", "cluster-a", "1")),
			},
			want: []string{"", ""},
		},
		{
			name: "a peer's GPU is in another clique",
			admissions: []group.Admission{
				sharedWith([]int{1}, inClique("GPU-a", "cluster-a", "1")),
				sharedWith([]int{0}, inClique("GPU-b", "cluster-a", "2")),
			},
			want: []string{
				refusal + "rank 1's GPU GPU-b is in NVLink clique 2, but rank 0's GPU GPU-a is in clique 1",
				refusal + "rank 1's GPU GPU-b is in NVLink clique 2, but rank 0's GPU GPU-a is in clique 1",
			},
		},
		{
			name: "a peer's GPU is in another cluster",
			admissions: []group.Admission{
				sharedWith([]int{1}, inClique("GPU-a", "cluster-a", "1")),
				sharedWith([]int{0}, inClique("GPU-b", "cluster-b", "1")),
			},
			want: []string{
				refusal + "rank 1's GPU GPU-b is in NVLink cluster cluster-b, but rank 0's GPU GPU-a is in cluster cluster-a",
				refusal + "rank 1's GPU GPU-b is in NVLink cluster cluster-b, but rank 0's GPU GPU-a is in cluster cluster-a",
			},
		},
		{
			name: "one of the member's own GPUs is not ready",
			admissions: []group.Admission{
				sharedWith([]int{1}, inClique("GPU-a", "cluster-a", "1"), notReady),
				sharedWith([]int{0}, inClique("GPU-b", "cluster-a", "1")),
			},
			want: []string{
				refusal + "rank 0's GPU GPU-a2 is not ready on its NVLink fabric (state In Progress, status N/A)",
				refusal + "rank 0's GPU GPU-a2 is not ready on its NVLink fabric (state In Progress, status N/A)",
			},
		},
		{
			name: "a peer reported no fabric",
			admissions: []group.Admission{
				sharedWith([]int{1}, inClique("GPU-a", "cluster-a", "1")),
				{},
			},
			want: []string{
				refusal + "rank 1 reported no NVLink fabric for its GPUs",
				refusal + "rank 1 reported no NVLink fabric for its GPUs",
			},
		},
		{
			// Rank 0 shared memory with rank 1 only, and rank 1 with rank 2
			// too. Each of them checks all three.
			name: "a peer of a peer is in another clique",
			admissions: []group.Admission{
				sharedWith([]int{1}, inClique("GPU-a", "cluster-a", "1")),
				sharedWith([]int{0, 2}, inClique("GPU-b", "cluster-a", "1")),
				sharedWith([]int{1}, inClique("GPU-c", "cluster-a", "2")),
			},
			want: []string{
				refusal + "rank 2's GPU GPU-c is in NVLink clique 2, but rank 0's GPU GPU-a is in clique 1",
				refusal + "rank 2's GPU GPU-c is in NVLink clique 2, but rank 0's GPU GPU-a is in clique 1",
				refusal + "rank 2's GPU GPU-c is in NVLink clique 2, but rank 0's GPU GPU-a is in clique 1",
			},
		},
		{
			// Rank 2 shared no GPU memory, so it reports no fabric and is not
			// checked, whatever the others report.
			name: "a member that shared no memory",
			admissions: []group.Admission{
				sharedWith([]int{1}, inClique("GPU-a", "cluster-a", "1")),
				sharedWith([]int{0}, inClique("GPU-b", "cluster-a", "2")),
				{},
			},
			want: []string{
				refusal + "rank 1's GPU GPU-b is in NVLink clique 2, but rank 0's GPU GPU-a is in clique 1",
				refusal + "rank 1's GPU GPU-b is in NVLink clique 2, but rank 0's GPU GPU-a is in clique 1",
				"",
			},
		},
		{
			name: "two pairs in two cliques",
			admissions: []group.Admission{
				sharedWith([]int{1}, inClique("GPU-a", "cluster-a", "1")),
				sharedWith([]int{0}, inClique("GPU-b", "cluster-a", "1")),
				sharedWith([]int{3}, inClique("GPU-c", "cluster-a", "2")),
				sharedWith([]int{2}, inClique("GPU-d", "cluster-a", "2")),
			},
			want: []string{"", "", "", ""},
		},
		{
			name:       "members that shared no memory",
			admissions: []group.Admission{{}, {}},
			want:       []string{"", ""},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for rank, want := range tc.want {
				err := checkFabric(rank, tc.admissions)
				if want == "" {
					assert.NoError(t, err, "rank %d", rank)
				} else {
					assert.EqualError(t, err, want, "rank %d", rank)
				}
			}
		})
	}
}

// sharedWith is the admission of a member whose Pod shared GPU memory over
// NVLink with peers', and whose restore container has gpus.
func sharedWith(peers []int, gpus ...types.GPUFabric) group.Admission {
	return group.Admission{Fabric: &group.FabricAdmission{Peers: peers, GPUs: gpus}}
}

// inClique is a GPU registered with its NVLink fabric, in clique of cluster.
func inClique(uuid, cluster, clique string) types.GPUFabric {
	return types.GPUFabric{UUID: uuid, ClusterUUID: cluster, CliqueID: clique, State: "Completed", Status: "Success"}
}

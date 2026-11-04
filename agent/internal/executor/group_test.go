// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

func TestGroupRestoreRefusesAnotherMembersArtifact(t *testing.T) {
	for _, tc := range []struct {
		name     string
		captured *types.GroupManifest
		want     string
	}{
		{name: "the member's own artifact", captured: &types.GroupManifest{Member: "worker", Rank: 1}},
		{name: "an artifact captured on its own", want: "not captured as a member of a pod group"},
		{
			name:     "another member's artifact",
			captured: &types.GroupManifest{Member: "leader", Rank: 0},
			want:     "the artifact is member leader at rank 0 of its capture, not member worker at rank 1",
		},
		{
			name:     "the member's artifact at another rank",
			captured: &types.GroupManifest{Member: "worker", Rank: 0},
			want:     "the artifact is member worker at rank 0 of its capture, not member worker at rank 1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkGroupArtifact(tc.captured, "worker", 1)
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.want)
		})
	}
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ai-dynamo/snapshot/agent/internal/cuda"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

func TestAllocationStorageManifestCompatibility(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, cuda.CuinterposeStateFile), []byte("state"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"", "host-carrier", "pagebroker", "future"} {
		manifest := &types.CheckpointManifest{
			Cuinterpose: types.CuinterposeManifest{Requested: true, Prepared: true, AllocationStorage: mode},
			CUDATools:   types.CUDAToolsManifest{Delivered: true},
		}
		manifest.CUDA.PIDs = []int{42}
		err := requireCuinterposeState(manifest, directory)
		if (err != nil) != (mode == "future") {
			t.Fatalf("mode=%q: %v", mode, err)
		}
	}
	manifest := &types.CheckpointManifest{Cuinterpose: types.CuinterposeManifest{AllocationStorage: "pagebroker"}}
	if err := requireCuinterposeState(manifest, directory); err == nil {
		t.Fatal("accepted external content without prepared shim state")
	}
}

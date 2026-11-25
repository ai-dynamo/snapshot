// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storageprobe

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func checkPVC(ctx context.Context, clusterID string, store PVC) error {
	base := path.Clean("/" + store.BasePath)
	if strings.Contains(base, "..") {
		return &Failure{Err: fmt.Errorf("base path %q leaves the claim", store.BasePath)}
	}
	directory := filepath.Join(store.MountPath, filepath.FromSlash(base), artifactsDirectory)
	file := filepath.Join(directory, accessCheckName(clusterID))

	if err := ctx.Err(); err != nil {
		return &Failure{Step: StepWrite, Err: err}
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return &Failure{Step: StepWrite, Err: err}
	}
	if err := os.WriteFile(file, accessCheckPayload, 0o644); err != nil {
		return &Failure{Step: StepWrite, Err: err}
	}
	got, err := os.ReadFile(file)
	if err != nil {
		return &Failure{Step: StepRead, Err: err}
	}
	if !bytes.Equal(got, accessCheckPayload) {
		return &Failure{Step: StepRead, Err: fmt.Errorf("the object read back differs from the one written")}
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return &Failure{Step: StepList, Err: err}
	}
	found := false
	for _, entry := range entries {
		if entry.Name() == accessCheckName(clusterID) {
			found = true
			break
		}
	}
	if !found {
		return &Failure{Step: StepList, Err: fmt.Errorf("the object is not in the listing")}
	}
	if err := os.Remove(file); err != nil {
		return &Failure{Step: StepDelete, Err: err}
	}
	return nil
}

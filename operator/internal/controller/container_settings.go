// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// copyContainerSettings deep-copies container settings from a request into
// the object it produces, so the two never share pointers.
func copyContainerSettings(in []snapshotv1alpha1.ContainerSnapshotConfig) []snapshotv1alpha1.ContainerSnapshotConfig {
	if in == nil {
		return nil
	}
	out := make([]snapshotv1alpha1.ContainerSnapshotConfig, len(in))
	for i := range in {
		in[i].DeepCopyInto(&out[i])
	}
	return out
}

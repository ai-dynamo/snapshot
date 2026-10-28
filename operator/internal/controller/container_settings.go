// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// podSnapshotReasonLifecycleHooksDisabled fails a PodSnapshot that asks for
// a checkpoint readiness probe or a post-restore action while they are off.
const podSnapshotReasonLifecycleHooksDisabled = "LifecycleHooksDisabled"

// errLifecycleHooksDisabled is the cause recorded when the gate rejects a
// request. The agent does not run these settings yet, so accepting them would
// checkpoint and restore as if they were absent.
//
// The gate, its flag and its chart value are temporary: they go away when the
// agent supports the settings (SNEP-411).
var errLifecycleHooksDisabled = errors.New("containers[].checkpointReadinessProbe and containers[].lifecycle " +
	"are not supported yet; the operator rejects them unless it runs with --enable-lifecycle-hooks " +
	"(chart value operator.lifecycleHooks)")

// usesLifecycleHooks reports whether any entry asks for behavior the gate
// controls. An entry with only a name changes nothing, so it is allowed.
func usesLifecycleHooks(containers []snapshotv1alpha1.ContainerSnapshotConfig) bool {
	for _, c := range containers {
		if c.CheckpointReadinessProbe != nil || c.Lifecycle != nil {
			return true
		}
	}
	return false
}

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

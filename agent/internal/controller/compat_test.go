// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/compat"
)

// A checkpoint whose manifest cannot be read is not incompatible, it is broken.
// The restore path reads the manifest again and reports that; refusing here
// would report the wrong outcome and hide the real error.
func TestPreflightCompatibilityAllowsUnreadableManifest(t *testing.T) {
	r := newGatedRestore(t, compat.Mismatch{Check: "kernel-version"})
	path := writeTestArtifact(t, r.controller.config.Storage.BasePath, "no-manifest-here", nil)

	err := r.controller.preflightCompatibility(r.pod, &restoreArtifact{
		ContainerName: gatedRestoreContainer,
		Path:          path,
	})

	require.NoError(t, err)
	assert.Empty(t, r.comparison.calls, "comparison ran without a manifest")
}

func TestPreflightCompatibilityComparesRecordedFacts(t *testing.T) {
	r := newGatedRestore(t)
	path := writeTestArtifact(t, r.controller.config.Storage.BasePath, "mounted-content", &types.CheckpointManifest{
		Artifact: types.ArtifactManifest{ContentUID: "mounted-content", ContainerName: gatedRestoreContainer},
		CRIUDump: types.CRIUDumpManifest{
			ExtMnt: map[string]string{
				"/model-cache": "/model-cache",
				"/etc/hosts":   "/etc/hosts",
			},
		},
	})

	err := r.controller.preflightCompatibility(r.pod, &restoreArtifact{
		ContainerName: gatedRestoreContainer,
		Path:          path,
	})

	require.NoError(t, err)
	require.Len(t, r.comparison.calls, 1)
	assert.Equal(t, compat.GatePreflight, r.comparison.calls[0].gate)
	assert.Equal(t, []string{"/etc/hosts", "/model-cache"}, r.comparison.calls[0].source.ExternalizedMounts)
}

// A refusal from the second gate is not a CRIU failure, so it neither reports
// one nor kills the placeholder: killing it would restart the container straight
// back into the same answer.
func TestRunRestoreTreatsIncompatibleAsTerminal(t *testing.T) {
	r := newGatedRestore(t)
	rt := &fakeRuntime{}
	r.controller.runtime = rt
	sentinels := 0
	r.controller.writeControlSentinelFn = func(int, string) error {
		sentinels++
		return nil
	}
	r.controller.restoreFn = refuseWith(compat.Mismatch{Check: "cpu-arch", Source: "amd64", Target: "arm64"})

	requeue := r.runRestore(t)

	assert.False(t, requeue, "a refusal asked to be driven again")
	assert.Empty(t, r.events(t, restoreFailedReason), "refusal reported itself as a restore failure")
	assert.Zero(t, sentinels, "refusal released the workload")
	assert.Empty(t, rt.resolvedContainerIDs, "refusal reached the placeholder kill path")
}

// The gate runs in preflight, before the restore is entered at all, so a refusal
// leaves no in-flight entry and no restore worker behind.
func TestReconcileRestorePodRefusesBeforeEnteringRestore(t *testing.T) {
	r := newGatedRestore(t, compat.Mismatch{Check: "memory-limit", Source: "32Gi", Target: "1Gi"})
	r.controller.restoreFn = func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (int, error) {
		t.Error("a refused restore was entered")
		return 0, nil
	}

	r.reconcile(t)

	assert.Len(t, r.comparison.calls, 1)
	assert.Empty(t, r.events(t, restoreRequestedReason), "refused restore still announced a request")
	assert.Empty(t, r.controller.inFlight, "refused restore claimed an attempt")
}

func refuseWith(mismatches ...compat.Mismatch) func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (int, error) {
	return func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (int, error) {
		return 0, compat.NewIncompatibleError(compat.GateInspect, mismatches)
	}
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/go-logr/logr/testr"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

// testMountPoint satisfies nsmount.MountPoint for executor unit tests.
type testMountPoint struct{ dst string }

func (m testMountPoint) Path(name string) (string, error) { return m.dst + "/" + name, nil }
func (m testMountPoint) Unmount(_ context.Context) error  { return nil }
func (m testMountPoint) NsFd() *os.File                   { return nil }

var _ nsmount.MountPoint = testMountPoint{}

type restoreFakeRuntime struct {
	resolvedID      string
	resolveByPodHit bool
}

func (r *restoreFakeRuntime) ResolveContainer(ctx context.Context, id string) (int, *specs.Spec, error) {
	r.resolvedID = id
	return 123, &specs.Spec{}, nil
}

func (r *restoreFakeRuntime) ResolveContainerIDByPod(ctx context.Context, pod, ns, ctr string) (string, error) {
	return "", errors.New("pod lookup should not be used")
}

func (r *restoreFakeRuntime) ResolveContainerByPod(ctx context.Context, pod, ns, ctr string) (int, *specs.Spec, error) {
	r.resolveByPodHit = true
	return 0, nil, errors.New("pod lookup should not be used")
}

func (r *restoreFakeRuntime) Close() error { return nil }

func TestInspectRestoreUsesContainerIDWhenProvided(t *testing.T) {
	manifest := types.NewCheckpointManifest(
		"checkpoint-123",
		types.CRIUDumpManifest{},
		types.NewSourcePodManifest("source-id", 456, "node-1", "source-pod", "default", "10.0.0.11", nil),
		types.OverlayManifest{},
	)
	rt := &restoreFakeRuntime{}
	_, err := inspectRestore(
		context.Background(),
		rt,
		testr.New(t),
		RestoreRequest{
			CheckpointID:  "checkpoint-123",
			ContainerID:   "placeholder-id",
			PodName:       "virtual-pod-name",
			PodNamespace:  "default",
			ContainerName: "main",
		},
		manifest,
	)
	if err != nil {
		t.Fatalf("inspectRestore: %v", err)
	}
	if rt.resolvedID != "placeholder-id" {
		t.Fatalf("ResolveContainer called with %q, want placeholder-id", rt.resolvedID)
	}
	if rt.resolveByPodHit {
		t.Fatal("ResolveContainerByPod should not be used when ContainerID is provided")
	}
}

func TestSetCleanupErrorIfSuccessful(t *testing.T) {
	cleanupErr := errors.New("unmount failed")

	t.Run("reports cleanup failure after successful restore", func(t *testing.T) {
		var retErr error
		setCleanupErrorIfSuccessful(&retErr, "unmount artifact", cleanupErr)
		if !errors.Is(retErr, cleanupErr) || !strings.Contains(retErr.Error(), "unmount artifact") {
			t.Fatalf("cleanup error = %v", retErr)
		}
	})

	t.Run("preserves existing restore failure", func(t *testing.T) {
		restoreErr := errors.New("restore failed")
		retErr := restoreErr
		setCleanupErrorIfSuccessful(&retErr, "unmount artifact", cleanupErr)
		if retErr != restoreErr {
			t.Fatalf("cleanup replaced restore error: %v", retErr)
		}
	})
}

func TestRestoreInNamespaceRejectsMultiGPUCheckpointWithoutLaunchJobState(t *testing.T) {
	checkpointDir := t.TempDir()
	manifest := types.NewCheckpointManifest(
		"checkpoint-123",
		types.CRIUDumpManifest{},
		types.NewSourcePodManifest("source-id", 456, "node-1", "source-pod", "default", "10.0.0.11", nil),
		types.OverlayManifest{},
	)
	manifest.CUDA = types.NewCUDAManifest([]int{42, 43}, []string{"GPU-aaa", "GPU-bbb"})
	if err := types.WriteManifest(checkpointDir, manifest); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	_, err := RestoreInNamespace(context.Background(), RestoreOptions{CheckpointPath: checkpointDir}, testr.New(t))
	if err == nil || !strings.Contains(err.Error(), "missing CUDA launch-job state") {
		t.Fatalf("expected missing multi-GPU launch-job error, got %v", err)
	}
}

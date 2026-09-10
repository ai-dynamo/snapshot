// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/testr"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/ai-dynamo/snapshot/agent/internal/cuda"
	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

// testMountPoint satisfies nsmount.MountPoint for executor unit tests.
type testMountPoint struct{}

func (m testMountPoint) Unmount(context.Context) error { return nil }
func (m testMountPoint) NsFd() *os.File                { return nil }

var _ nsmount.MountPoint = testMountPoint{}

type restoreFakeRuntime struct {
	resolvedID             string
	resolvedByPodContainer string
	resolveByPodHit        bool
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
	r.resolvedByPodContainer = ctr
	return 0, nil, errors.New("pod lookup should not be used")
}

func (r *restoreFakeRuntime) Close() error { return nil }

func TestInspectRestoreUsesContainerIDWhenProvided(t *testing.T) {
	manifest := types.NewCheckpointManifest(
		"content-uid-123",
		"main",
		types.CRIUDumpManifest{},
		types.NewSourcePodManifest("source-id", 456, "node-1", "source-pod", "default", "10.0.0.11", nil),
		types.OverlayManifest{},
	)
	rt := &restoreFakeRuntime{}
	_, _, err := inspectRestore(
		context.Background(),
		rt,
		testr.New(t),
		RestoreRequest{
			ContentUID:               "content-uid-123",
			ContainerID:              "placeholder-id",
			PodName:                  "virtual-pod-name",
			PodNamespace:             "default",
			ArtifactContainerName:    "main",
			DestinationContainerName: "engine-0",
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

func TestInspectRestoreUsesDestinationNameForPodLookup(t *testing.T) {
	manifest := types.NewCheckpointManifest(
		"content-uid-123",
		"main",
		types.CRIUDumpManifest{},
		types.NewSourcePodManifest("source-id", 456, "node-1", "source-pod", "default", "10.0.0.11", nil),
		types.OverlayManifest{},
	)
	rt := &restoreFakeRuntime{}
	_, _, err := inspectRestore(
		context.Background(),
		rt,
		testr.New(t),
		RestoreRequest{
			ContentUID:               "content-uid-123",
			PodName:                  "virtual-pod-name",
			PodNamespace:             "default",
			ArtifactContainerName:    "main",
			DestinationContainerName: "engine-0",
		},
		manifest,
	)
	if err == nil {
		t.Fatal("inspectRestore should report the fake pod lookup error")
	}
	if rt.resolvedByPodContainer != "engine-0" {
		t.Fatalf("ResolveContainerByPod called with container %q, want engine-0", rt.resolvedByPodContainer)
	}
}

func TestNewRestoreCleanupError(t *testing.T) {
	cleanupErr := errors.New("unmount failed")
	retErr := NewRestoreCleanupError(fmt.Errorf("unmount artifact: %w", cleanupErr))
	if !errors.Is(retErr, cleanupErr) || !strings.Contains(retErr.Error(), "unmount artifact") {
		t.Fatalf("cleanup error = %v", retErr)
	}
	var typedErr *RestoreCleanupError
	if !errors.As(retErr, &typedErr) {
		t.Fatalf("cleanup error type = %T, want *RestoreCleanupError", retErr)
	}
}

func TestValidateRestoreManifest(t *testing.T) {
	manifest := types.NewCheckpointManifest(
		"content-uid-123",
		"main",
		types.CRIUDumpManifest{},
		types.NewSourcePodManifest("source-id", 456, "node-1", "source-pod", "team-a", "10.0.0.11", nil),
		types.OverlayManifest{},
	)

	for _, tc := range []struct {
		name string
		req  RestoreRequest
		want string
	}{
		{name: "matching identity", req: RestoreRequest{ContentUID: "content-uid-123", ArtifactContainerName: "main", DestinationContainerName: "engine-0"}},
		{
			name: "content UID mismatch",
			req:  RestoreRequest{ContentUID: "other", ArtifactContainerName: "main", DestinationContainerName: "engine-0"},
			want: "does not match requested artifact",
		},
		{
			name: "container mismatch",
			req:  RestoreRequest{ContentUID: "content-uid-123", ArtifactContainerName: "worker", DestinationContainerName: "engine-0"},
			want: "does not match requested artifact",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRestoreManifest(tc.req, manifest)
			if tc.want == "" && err != nil {
				t.Fatalf("validateRestoreManifest() error = %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("validateRestoreManifest() error = %v, want substring %q", err, tc.want)
			}
		})
	}

	manifest.CUDA = types.NewCUDAManifest(
		[]int{42, 43},
		[]string{"GPU-aaa", "GPU-bbb"},
		types.CUDAStorageModePOSIX,
	)
	err := validateRestoreManifest(
		RestoreRequest{ContentUID: "content-uid-123", ArtifactContainerName: "main", DestinationContainerName: "engine-0", PodNamespace: "team-a"},
		manifest,
	)
	if err != nil {
		t.Fatalf("validateRestoreManifest(multi-GPU POSIX) = %v, want supported manifest", err)
	}
}

func TestRestoreInNamespaceRejectsMultiGPUCheckpointWithoutLaunchJobState(t *testing.T) {
	checkpointDir := t.TempDir()
	manifest := types.NewCheckpointManifest(
		"content-uid-123",
		"main",
		types.CRIUDumpManifest{},
		types.NewSourcePodManifest("source-id", 456, "node-1", "source-pod", "default", "10.0.0.11", nil),
		types.OverlayManifest{},
	)
	manifest.CUDA = types.NewCUDAManifest([]int{42, 43}, []string{"GPU-aaa", "GPU-bbb"}, types.CUDAStorageModeLegacy)
	if err := types.WriteManifest(checkpointDir, manifest); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	_, err := RestoreInNamespace(context.Background(), RestoreOptions{CheckpointPath: checkpointDir}, testr.New(t))
	if err == nil || !strings.Contains(err.Error(), "missing CUDA launch-job state") {
		t.Fatalf("expected missing multi-GPU launch-job error, got %v", err)
	}
}

func TestRemainingDuration(t *testing.T) {
	got := remainingDuration(10*time.Second, 4*time.Second, 3*time.Second)
	if got != 3*time.Second {
		t.Fatalf("remainingDuration = %s, want 3s", got)
	}
	if remainingDuration(5*time.Second, 4*time.Second, 3*time.Second) != 0 {
		t.Fatal("remainingDuration should not go negative")
	}
}

func TestRestoreDeferredCUDAProcessesResolvesAndValidatesHostIdentity(t *testing.T) {
	namespaceProcess := snapshotruntime.ProcessDetails{
		InnermostPID:   7,
		StartTimeTicks: 101,
		Cgroup:         "0::/restored\n",
	}
	hostProcess := snapshotruntime.ProcessDetails{
		ObservedPID:    9007,
		OutermostPID:   9007,
		InnermostPID:   7,
		NamespacePIDs:  []int{9007, 7},
		StartTimeTicks: 101,
		Cgroup:         "0::/restored\n",
	}

	originalRead := readRestoredHostProcessTable
	originalValidate := validateRestoredProcessIdentity
	originalRestore := restoreAndUnlockCUDAProcessTree
	originalCuinterpose := restoreCuinterposeFromHost
	t.Cleanup(func() {
		readRestoredHostProcessTable = originalRead
		validateRestoredProcessIdentity = originalValidate
		restoreAndUnlockCUDAProcessTree = originalRestore
		restoreCuinterposeFromHost = originalCuinterpose
	})

	readRestoredHostProcessTable = func(procRoot string) ([]snapshotruntime.ProcessDetails, error) {
		if procRoot != snapshotruntime.HostProcPath {
			t.Fatalf("ReadProcessTable proc root = %q, want %q", procRoot, snapshotruntime.HostProcPath)
		}
		return []snapshotruntime.ProcessDetails{hostProcess}, nil
	}
	validated := false
	validateRestoredProcessIdentity = func(procRoot string, process snapshotruntime.ProcessDetails) error {
		if procRoot != snapshotruntime.HostProcPath || process.OutermostPID != hostProcess.OutermostPID {
			t.Fatalf("ValidateProcessIdentity(%q, PID %d), want (%q, PID %d)",
				procRoot, process.OutermostPID, snapshotruntime.HostProcPath, hostProcess.OutermostPID)
		}
		validated = true
		return nil
	}
	order := make([]string, 0, 2)
	restoreAndUnlockCUDAProcessTree = func(
		ctx context.Context,
		processes []snapshotruntime.ProcessDetails,
		deviceMap, storageMode, checkpointDir, jobFile string,
		targetGPUUUIDs []string,
		transferSettings types.CUDATransferSettings,
		_ logr.Logger,
	) (cuda.RestorePhaseTimings, error) {
		if len(processes) != 1 || processes[0].OutermostPID != hostProcess.OutermostPID {
			t.Fatalf("CUDA restore processes = %+v, want host PID %d", processes, hostProcess.OutermostPID)
		}
		if deviceMap != "0=1" || storageMode != types.CUDAStorageModePOSIX || checkpointDir == "" {
			t.Fatalf("CUDA restore args = device map %q, storage mode %q, checkpoint dir %q", deviceMap, storageMode, checkpointDir)
		}
		if len(targetGPUUUIDs) != 1 || targetGPUUUIDs[0] != "GPU-target" {
			t.Fatalf("target GPU UUIDs = %v", targetGPUUUIDs)
		}
		order = append(order, "cuda")
		return cuda.RestorePhaseTimings{TotalDuration: 250 * time.Millisecond}, nil
	}
	restoreCuinterposeFromHost = func(
		ctx context.Context, checkpointDir, procRoot string, observedPIDs, namespacePIDs []int,
		binary string, _ logr.Logger,
	) ([]cuda.CoordinatorPhase, error) {
		if procRoot != snapshotruntime.HostProcPath || len(observedPIDs) != 1 || observedPIDs[0] != hostProcess.ObservedPID {
			t.Fatalf("cuinterpose host args = proc root %q, observed PIDs %v", procRoot, observedPIDs)
		}
		if len(namespacePIDs) != 1 || namespacePIDs[0] != 7 || binary != cuda.DefaultCoordinatorBinaryPath {
			t.Fatalf("cuinterpose namespace PIDs %v, binary %q", namespacePIDs, binary)
		}
		order = append(order, "cuinterpose")
		return nil, nil
	}

	manifest := &types.CheckpointManifest{
		CUDA:        types.NewCUDAManifest([]int{7}, []string{"GPU-source"}, types.CUDAStorageModePOSIX),
		Cuinterpose: types.CuinterposeManifest{Prepared: true},
	}
	duration, _, err := restoreDeferredCUDAProcesses(
		context.Background(),
		[]snapshotruntime.ProcessDetails{namespaceProcess},
		&types.RestoreContainerSnapshot{
			CUDADeviceMap:   "0=1",
			CUDAStorageMode: types.CUDAStorageModePOSIX,
			TargetGPUUUIDs:  []string{"GPU-target"},
		},
		manifest,
		t.TempDir(),
		types.CUDATransferSettings{},
		testr.New(t),
	)
	if err != nil {
		t.Fatalf("restoreDeferredCUDAProcesses: %v", err)
	}
	if !validated || !slices.Equal(order, []string{"cuda", "cuinterpose"}) {
		t.Fatalf("validated = %t, order = %v; want CUDA before cuinterpose", validated, order)
	}
	if duration != 250*time.Millisecond {
		t.Fatalf("duration = %s, want 250ms", duration)
	}
}

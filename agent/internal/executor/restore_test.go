// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/ai-dynamo/snapshot/agent/internal/cuda"
	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/compat"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

func TestInspectCompatibilityManagedCUDAToolsMount(t *testing.T) {
	for _, tc := range []struct {
		name      string
		delivered bool
		mount     string
		wantError bool
	}{
		{"delivered tools installed later", true, podcontract.CUDAToolsMountPath, false},
		{"unmanaged tools still required", false, podcontract.CUDAToolsMountPath, true},
		{"workload mount still required", true, "/models", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest := &types.CheckpointManifest{}
			manifest.CUDATools.Delivered = tc.delivered
			manifest.CRIUDump.ExtMnt = map[string]string{tc.mount: tc.mount}
			err := inspectCompatibility(testr.New(t), manifest, compat.GPUInfo{}, t.TempDir(), "", false)
			if (err != nil) != tc.wantError {
				t.Fatalf("inspectCompatibility() = %v, wantError %v", err, tc.wantError)
			}
		})
	}
}

// testMountPoint satisfies nsmount.MountPoint for executor unit tests.
type testMountPoint struct{}

func (m testMountPoint) Unmount(context.Context) error { return nil }
func (m testMountPoint) NsFd() *os.File                { return nil }

var _ nsmount.MountPoint = testMountPoint{}

type restoreFakeRuntime struct {
	resolvedID             string
	resolvedByPodContainer string
	resolveByPodHit        bool
	imageID                string
	imageIDError           error
	imageIDHit             bool
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

func (r *restoreFakeRuntime) ResolveContainerImageID(_ context.Context, _ string) (string, error) {
	r.imageIDHit = true
	return r.imageID, r.imageIDError
}

func (r *restoreFakeRuntime) Close() error { return nil }

func TestInspectRestoreUsesContainerIDWhenProvided(t *testing.T) {
	manifest := types.NewCheckpointManifest(
		"content-uid-123",
		"main",
		types.CRIUDumpManifest{},
		types.NewSourcePodManifest("source-id", 456, "node-1", "source-pod", "default", "10.0.0.11", nil),
		types.OverlayManifest{},
		types.HostManifest{},
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

func TestInspectRestoreComparesRuntimeImageID(t *testing.T) {
	const (
		captured = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
		rebuilt  = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	)
	tests := []struct {
		name            string
		sourceID        string
		targetID        string
		targetError     error
		skipCompatCheck bool
		want            []compat.Mismatch
		wantNoLookup    bool
	}{
		{
			name:     "same runtime content",
			sourceID: captured,
			targetID: captured,
		},
		{
			name:     "different runtime content",
			sourceID: captured,
			targetID: rebuilt,
			want:     []compat.Mismatch{{Check: compat.CheckImageDigest, Source: captured, Target: rebuilt}},
		},
		{
			name:     "artifact without a runtime image ID",
			targetID: captured,
		},
		{
			name:     "target without a runtime image ID",
			sourceID: captured,
		},
		{
			name:        "runtime image ID unavailable is left uncompared",
			sourceID:    captured,
			targetError: errors.New("runtime unavailable"),
		},
		{
			name:            "skipped check never asks the runtime",
			sourceID:        captured,
			targetID:        rebuilt,
			skipCompatCheck: true,
			wantNoLookup:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			manifest := types.NewCheckpointManifest(
				"content-uid-123",
				"main",
				types.CRIUDumpManifest{},
				types.NewSourcePodManifest("source-id", 456, "node-1", "source-pod", "default", "10.0.0.11", nil),
				types.OverlayManifest{},
				types.HostManifest{},
			)
			manifest.K8s.ImageID = tc.sourceID
			rt := &restoreFakeRuntime{imageID: tc.targetID, imageIDError: tc.targetError}

			_, _, err := inspectRestore(
				context.Background(),
				rt,
				testr.New(t),
				RestoreRequest{
					ContentUID:               "content-uid-123",
					ContainerID:              "placeholder-id",
					PodName:                  "restore-pod",
					PodNamespace:             "default",
					ArtifactContainerName:    "main",
					DestinationContainerName: "main",
					SkipCompatCheck:          tc.skipCompatCheck,
				},
				manifest,
			)
			if tc.wantNoLookup && rt.imageIDHit {
				t.Fatal("resolved the runtime image ID while the check was skipped")
			}
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("inspectRestore: %v", err)
				}
				return
			}
			var incompatible *compat.IncompatibleError
			if !errors.As(err, &incompatible) {
				t.Fatalf("error = %v, want *compat.IncompatibleError", err)
			}
			if incompatible.Gate != compat.GateInspect {
				t.Fatalf("gate = %q, want %q", incompatible.Gate, compat.GateInspect)
			}
			if !reflect.DeepEqual(incompatible.Mismatches, tc.want) {
				t.Fatalf("mismatches = %+v, want %+v", incompatible.Mismatches, tc.want)
			}
		})
	}
}

func TestInspectRestoreUsesDestinationNameForPodLookup(t *testing.T) {
	manifest := types.NewCheckpointManifest(
		"content-uid-123",
		"main",
		types.CRIUDumpManifest{},
		types.NewSourcePodManifest("source-id", 456, "node-1", "source-pod", "default", "10.0.0.11", nil),
		types.OverlayManifest{},
		types.NewHostManifest("6.17.0"),
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
		types.HostManifest{},
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
}

func TestRestoreInNamespaceRejectsMultiGPUCheckpointWithoutLaunchJobState(t *testing.T) {
	checkpointDir := t.TempDir()
	manifest := types.NewCheckpointManifest(
		"content-uid-123",
		"main",
		types.CRIUDumpManifest{},
		types.NewSourcePodManifest("source-id", 456, "node-1", "source-pod", "default", "10.0.0.11", nil),
		types.OverlayManifest{},
		types.HostManifest{},
	)
	manifest.CUDA = types.NewCUDAManifest([]int{42, 43}, compat.GPUInfo{
		Devices: []compat.GPUDevice{{UUID: "GPU-aaa"}, {UUID: "GPU-bbb"}},
	})
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

func TestExistingMountPaths(t *testing.T) {
	targetRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(targetRoot, "model-cache"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(targetRoot, "etc-hostname"), nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got := existingMountPaths(targetRoot, []string{"/model-cache", "/data", "/etc-hostname"})
	want := []string{"/model-cache", "/etc-hostname"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("existingMountPaths = %#v, want %#v", got, want)
	}

	if got := existingMountPaths(targetRoot, nil); len(got) != 0 {
		t.Errorf("existingMountPaths of nothing = %#v, want empty", got)
	}
}

// recordingMounter records the order of role mounts for mountRestoreInputs.
type recordingMounter struct {
	calls   []string
	failOn  string
	failErr error
}

func (m *recordingMounter) record(role string) (nsmount.MountPoint, error) {
	m.calls = append(m.calls, role)
	if role == m.failOn {
		return nil, m.failErr
	}
	return testMountPoint{}, nil
}

func (m *recordingMounter) MountBundle(context.Context, int) (nsmount.MountPoint, error) {
	return m.record("bundle")
}

func (m *recordingMounter) MountCUDATools(context.Context, nsmount.MountPoint) (nsmount.MountPoint, error) {
	return m.record("cudatools")
}

func (m *recordingMounter) MountArtifact(context.Context, nsmount.MountPoint, string) (nsmount.MountPoint, error) {
	return m.record("artifact")
}

func (m *recordingMounter) MountPageBroker(context.Context, nsmount.MountPoint, string) (nsmount.MountPoint, error) {
	return m.record("pagebroker")
}

func TestMountRestoreInputsOrdersCUDAToolsBeforeStaging(t *testing.T) {
	cases := map[string]struct {
		tools     bool
		staged    string
		wantCalls []string
		wantPath  string
	}{
		"plain artifact":      {wantCalls: []string{"artifact"}, wantPath: nsmount.CheckpointDst},
		"tools then artifact": {tools: true, wantCalls: []string{"cudatools", "artifact"}, wantPath: nsmount.CheckpointDst},
		"brokered":            {staged: "/pagebroker/staging/restore/tx", wantCalls: []string{"pagebroker"}, wantPath: nsmount.PageBrokerDst},
		"tools then brokered": {tools: true, staged: "/pagebroker/staging/restore/tx", wantCalls: []string{"cudatools", "pagebroker"}, wantPath: nsmount.PageBrokerDst},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := &recordingMounter{}
			mounted, path, err := mountRestoreInputs(context.Background(), m, tc.tools, testMountPoint{}, "/checkpoints/x", tc.staged)
			if err != nil {
				t.Fatalf("mountRestoreInputs: %v", err)
			}
			if strings.Join(m.calls, ",") != strings.Join(tc.wantCalls, ",") {
				t.Fatalf("mount order = %v, want %v", m.calls, tc.wantCalls)
			}
			if path != tc.wantPath {
				t.Fatalf("checkpoint path = %q, want %q", path, tc.wantPath)
			}
			if len(mounted) != len(tc.wantCalls) {
				t.Fatalf("mounted %d, want %d", len(mounted), len(tc.wantCalls))
			}
			// The brokered path unmounts the last active mount early; it must
			// be the staging mount, never the tools mount.
			if tc.staged != "" && mounted[len(mounted)-1].action != "unmount PageBroker staging from placeholder" {
				t.Fatalf("last mount = %q, want the staging mount", mounted[len(mounted)-1].action)
			}
		})
	}
}

func TestMountRestoreInputsReturnsEarlierMountsOnFailure(t *testing.T) {
	m := &recordingMounter{failOn: "artifact", failErr: errors.New("boom")}
	mounted, _, err := mountRestoreInputs(context.Background(), m, true, testMountPoint{}, "/checkpoints/x", "")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected the artifact mount error, got %v", err)
	}
	if len(mounted) != 1 || mounted[0].action != "unmount CUDA tools from placeholder" {
		t.Fatalf("earlier mounts must be returned for cleanup, got %+v", mounted)
	}
}

func TestRequireCuinterposeState(t *testing.T) {
	dir := t.TempDir()
	plain := &types.CheckpointManifest{}
	if err := requireCuinterposeState(plain, dir); err != nil {
		t.Fatalf("a checkpoint without cuinterpose needs no state file: %v", err)
	}
	prepared := &types.CheckpointManifest{
		CUDA:        types.NewCUDAManifest([]int{1}, compat.GPUInfo{}),
		CUDATools:   types.CUDAToolsManifest{Delivered: true},
		Cuinterpose: types.CuinterposeManifest{Requested: true, Prepared: true},
	}
	if err := requireCuinterposeState(prepared, dir); err == nil {
		t.Fatal("a prepared checkpoint without its state file must be refused")
	}
	if err := os.WriteFile(dir+"/"+cuda.CuinterposeStateFile, []byte("cuinterpose-state-v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := requireCuinterposeState(prepared, dir); err != nil {
		t.Fatalf("state present: %v", err)
	}
	prepared.CUDATools.Delivered = false
	if err := requireCuinterposeState(prepared, dir); err == nil {
		t.Fatal("prepared without delivered tools must not take the native path")
	}
	prepared.CUDATools.Delivered = true
	prepared.Cuinterpose.Requested = false
	if err := requireCuinterposeState(prepared, dir); err == nil {
		t.Fatal("prepared without requested interposition is inconsistent")
	}
	noCUDA := &types.CheckpointManifest{Cuinterpose: types.CuinterposeManifest{Prepared: true}}
	if err := requireCuinterposeState(noCUDA, dir); err == nil {
		t.Fatal("prepared without CUDA processes is inconsistent")
	}
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"

	"github.com/ai-dynamo/snapshot/agent/internal/criu"
	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/compat"
	"github.com/ai-dynamo/snapshot/api/podcontract"
	"google.golang.org/protobuf/proto"
)

func TestRestoreUsesSavedCUDAFormat(t *testing.T) {
	for _, engine := range []bool{false, true} {
		for _, requested := range []bool{false, true} {
			for _, storage := range []string{"cpu", "driver", "custom"} {
				t.Run(fmt.Sprintf("engine-%t/requested-%t/%s", engine, requested, storage), func(t *testing.T) {
					base := t.TempDir()
					directory, err := nsmount.ResolveArtifactPath(base, "content", "main")
					if err != nil {
						t.Fatal(err)
					}
					if err := os.MkdirAll(directory, 0700); err != nil {
						t.Fatal(err)
					}
					manifest := types.NewCheckpointManifest("content", "main", types.CRIUDumpManifest{},
						types.SourcePodManifest{}, types.OverlayManifest{}, types.HostManifest{})
					if storage != "cpu" {
						manifest.CUDA = types.CUDAManifest{PIDs: []int{12}, CustomStorage: storage == "custom"}
						if err := os.WriteFile(filepath.Join(directory, podcontract.CUDAJobFileName), []byte("launch-state"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					if err := types.WriteManifest(directory, manifest); err != nil {
						t.Fatal(err)
					}
					log := testr.New(t)
					_, err = Restore(context.Background(), checkpointPathRuntime{}, log, RestoreRequest{
						BasePath: base, ContentUID: "content", ArtifactContainerName: "main", ContainerID: "placeholder",
						PageBrokerEnabled: engine, PageBrokerRequested: requested,
						CustomStorageAvailable: true,
					}, nsmount.New(log))
					// Restore uses startup capabilities without contacting PageBroker.
					want := "stop after path preparation"
					if storage == "custom" && engine && !requested {
						want = "CustomStorage restore cannot opt out of PageBroker with nvidia.com/snapshot-pagebroker=false"
					}
					if storage == "custom" && !engine {
						want = "CustomStorage checkpoint requires PageBroker"
					}
					if err == nil || !strings.Contains(err.Error(), want) {
						t.Fatalf("restore error=%v; want %s", err, want)
					}
				})
			}
		}
	}
}

func TestCustomStorageRestoreRequiresExecutionSocket(t *testing.T) {
	directory := t.TempDir()
	manifest := types.NewCheckpointManifest("content", "main", types.CRIUDumpManifest{},
		types.SourcePodManifest{}, types.OverlayManifest{}, types.HostManifest{})
	manifest.CUDA = types.CUDAManifest{PIDs: []int{12}, CustomStorage: true}
	if err := types.WriteManifest(directory, manifest); err != nil {
		t.Fatal(err)
	}
	_, err := RestoreInNamespace(context.Background(), RestoreOptions{CheckpointPath: directory}, testr.New(t))
	if err == nil || !strings.Contains(err.Error(), "CustomStorage checkpoint requires PageBroker execution") {
		t.Fatalf("unexpected missing-session result: %v", err)
	}
}

func TestDriverRestoreRejectsCustomStorageExecution(t *testing.T) {
	directory := t.TempDir()
	manifest := types.NewCheckpointManifest("content", "main", types.CRIUDumpManifest{},
		types.SourcePodManifest{}, types.OverlayManifest{}, types.HostManifest{})
	if err := types.WriteManifest(directory, manifest); err != nil {
		t.Fatal(err)
	}
	_, err := RestoreInNamespace(context.Background(), RestoreOptions{
		CheckpointPath:         directory,
		CustomStorageExecution: &pagebroker.CustomStorageExecution{},
	}, testr.New(t))
	if err == nil || !strings.Contains(err.Error(), "checkpoint does not use CustomStorage") {
		t.Fatalf("unexpected format mismatch: %v", err)
	}
}

func TestInspectCompatibilityChecksMappedGPUMountAndOrdinaryMounts(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dev"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dev/nvidia1"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	manifest := &types.CheckpointManifest{
		CUDA: types.CUDAManifest{
			SourceGPUUUIDs: []string{"GPU-source"},
			DevicePaths:    map[string]string{"GPU-source": "/dev/nvidia0"},
		},
		CRIUDump: types.CRIUDumpManifest{ExtMnt: map[string]string{"/dev/nvidia0": "/dev/nvidia0"}},
	}
	target := compat.GPUInfo{Devices: []compat.GPUDevice{{UUID: "GPU-target"}}}
	paths := map[string]string{"GPU-target": "/dev/nvidia1"}
	check := func() error {
		_, aliases, err := prepareGPUMapping(testr.New(t), manifest, []string{"GPU-target"},
			func() (map[string]string, error) { return paths, nil })
		if err != nil {
			return err
		}
		if aliases["/dev/nvidia0"] != "/dev/nvidia1" {
			t.Fatalf("restore plan = %v", aliases)
		}
		return inspectCompatibility(testr.New(t), manifest, target, aliases, root, "", false)
	}
	if err := check(); err != nil {
		t.Fatalf("validated destination alias refused: %v", err)
	}
	manifest.CRIUDump.ExtMnt["/missing-data"] = "/missing-data"
	if err := check(); err == nil {
		t.Fatal("missing ordinary mount was accepted")
	}
	delete(manifest.CRIUDump.ExtMnt, "/missing-data")
	delete(paths, "GPU-target")
	if err := check(); err == nil {
		t.Fatal("missing GPU mount without a validated destination was accepted")
	}
}

func TestGPUMappingLeavesCountPolicyToInspectGate(t *testing.T) {
	manifest := &types.CheckpointManifest{
		CUDA: types.CUDAManifest{SourceGPUUUIDs: []string{"GPU-source"}},
	}
	target := compat.GPUInfo{Devices: []compat.GPUDevice{{UUID: "GPU-a"}, {UUID: "GPU-b"}}}
	deviceMap, aliases, err := prepareGPUMapping(testr.New(t), manifest, []string{"GPU-a", "GPU-b"},
		func() (map[string]string, error) {
			t.Fatal("count mismatch should not resolve device paths")
			return nil, nil
		})
	if err != nil || deviceMap != "" || len(aliases) != 0 {
		t.Fatalf("mapping preparation = %q, %v, %v", deviceMap, aliases, err)
	}
	err = inspectCompatibility(testr.New(t), manifest, target, aliases, t.TempDir(), "", false)
	var incompatible *compat.IncompatibleError
	if !errors.As(err, &incompatible) || incompatible.Gate != compat.GateInspect {
		t.Fatalf("expected registered inspect refusal, got %v", err)
	}
	if len(incompatible.Mismatches) != 1 || incompatible.Mismatches[0].Check != compat.CheckGPUCount {
		t.Fatalf("expected GPU count check, got %+v", incompatible.Mismatches)
	}
}

func TestInspectCompatibilityManagedCuInterposeMount(t *testing.T) {
	for _, tc := range []struct {
		name      string
		delivered bool
		mount     string
		wantError bool
	}{
		{"delivered tools installed later", true, podcontract.CuInterposeMountPath, false},
		{"unmanaged tools still required", false, podcontract.CuInterposeMountPath, true},
		{"workload mount still required", true, "/models", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest := &types.CheckpointManifest{}
			if tc.delivered {
				manifest.CuInterpose = testCuInterposeIdentity()
			}
			manifest.CRIUDump.ExtMnt = map[string]string{tc.mount: tc.mount}
			err := inspectCompatibility(testr.New(t), manifest, compat.GPUInfo{}, nil, t.TempDir(), "", false)
			if (err != nil) != tc.wantError {
				t.Fatalf("inspectCompatibility() = %v, wantError %v", err, tc.wantError)
			}
		})
	}
}

// testMountPoint satisfies nsmount.MountPoint for executor unit tests.
type testMountPoint struct {
	t          *testing.T
	name       string
	calls      *[]string
	releaseErr error
}

func (m testMountPoint) Unmount(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		m.t.Errorf("cleanup context canceled: %v", err)
	}
	*m.calls = append(*m.calls, "unmount "+m.name)
	return nil
}

func (m testMountPoint) Release() error {
	*m.calls = append(*m.calls, "release "+m.name)
	return m.releaseErr
}

func (m testMountPoint) NsFd() *os.File { return nil }

var _ nsmount.MountPoint = testMountPoint{}

func TestCleanupRestoreMountsRetainsLibrariesOnlyAfterSuccess(t *testing.T) {
	closeErr := errors.New("close namespace fd")
	for _, tc := range []struct {
		name       string
		restored   bool
		releaseErr error
		want       []string
	}{
		{"restore failed", false, nil, []string{"unmount artifact", "unmount shim", "unmount bundle"}},
		{"restored", true, nil, []string{"unmount artifact", "release shim", "unmount bundle"}},
		{"release failed", true, closeErr, []string{"unmount artifact", "release shim", "unmount bundle"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			mounts := []restoreMount{
				{point: testMountPoint{t: t, name: "bundle", calls: &calls}},
				{point: testMountPoint{t: t, name: "shim", calls: &calls, releaseErr: tc.releaseErr}, keepOnSuccess: true},
				{point: testMountPoint{t: t, name: "artifact", calls: &calls}},
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := cleanupRestoreMounts(ctx, mounts, tc.restored)
			if !errors.Is(err, tc.releaseErr) {
				t.Fatalf("cleanup error = %v, want %v", err, tc.releaseErr)
			}
			if !reflect.DeepEqual(calls, tc.want) {
				t.Fatalf("cleanup calls = %v, want %v", calls, tc.want)
			}
		})
	}
}

type restoreFakeRuntime struct {
	resolvedID             string
	resolvedByPodContainer string
	resolveByPodHit        bool
	imageID                string
	imageIDError           error
	imageIDHit             bool
	env                    []string
}

func (r *restoreFakeRuntime) ResolveContainer(ctx context.Context, id string) (int, *specs.Spec, error) {
	r.resolvedID = id
	return 123, &specs.Spec{Process: &specs.Process{Env: r.env}}, nil
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

func (r *restoreFakeRuntime) TerminateContainer(context.Context, string) error {
	return errors.New("not implemented")
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

func TestRestoreInNamespaceJobFileRequirement(t *testing.T) {
	for _, tc := range []struct {
		name            string
		cuInterposePIDs []int // nil means native CUDA, empty means frontend only
		wantStopsAt     string
	}{
		{name: "native multi-GPU missing", wantStopsAt: "multi-GPU CUDA checkpoint is missing"},
		{name: "frontend-only multi-GPU missing", cuInterposePIDs: []int{}, wantStopsAt: "multi-GPU CUDA checkpoint is missing"},
		{name: "coordinator multi-GPU missing", cuInterposePIDs: []int{43}, wantStopsAt: "invalid target pod IP"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkpointDir := t.TempDir()
			manifest := types.NewCheckpointManifest(
				"content-uid-123", "main", types.CRIUDumpManifest{},
				types.NewSourcePodManifest("source-id", 456, "node-1", "source-pod", "default", "10.0.0.11", nil),
				types.OverlayManifest{}, types.HostManifest{},
			)
			manifest.CUDA.PIDs = []int{42, 43}
			manifest.CUDA.SourceGPUUUIDs = []string{"GPU-aaa", "GPU-bbb"}
			if tc.cuInterposePIDs != nil {
				manifest.CuInterpose = testCuInterposeIdentity()
				manifest.CuInterpose.PIDs = tc.cuInterposePIDs
			}
			// Stopping at IP validation exercises jobfile selection in the real restore
			// preflight without reaching namespace or CUDA operations.
			manifest.CRIUDump.CRIU.TcpEstablished = true
			t.Setenv(criu.InetRemapEnvVar, "")
			if err := types.WriteManifest(checkpointDir, manifest); err != nil {
				t.Fatal(err)
			}
			_, err := RestoreInNamespace(context.Background(), RestoreOptions{
				CheckpointPath: checkpointDir, TargetPodIP: "invalid",
			}, testr.New(t))
			if err == nil || !strings.Contains(err.Error(), tc.wantStopsAt) {
				t.Fatalf("RestoreInNamespace() = %v, want preflight to stop at %q", err, tc.wantStopsAt)
			}
		})
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

	got := existingMountPaths(targetRoot, []string{"/model-cache", "/data", "/etc-hostname"}, nil)
	want := []string{"/model-cache", "/etc-hostname"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("existingMountPaths = %#v, want %#v", got, want)
	}

	if got := existingMountPaths(targetRoot, nil, nil); len(got) != 0 {
		t.Errorf("existingMountPaths of nothing = %#v, want empty", got)
	}
}

type restoreSourceMounter struct {
	bundleDone chan struct{}
	artifact   string
	staged     string
	fail       bool
	cancel     context.CancelFunc
	events     *[]string
}

type restoreSourceMount struct {
	done   chan struct{}
	name   string
	events *[]string
}

func (m restoreSourceMount) NsFd() *os.File { return nil }
func (m restoreSourceMount) Release() error { return nil }
func (m restoreSourceMount) Unmount(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	*m.events = append(*m.events, "unmount "+m.name)
	if m.done != nil {
		close(m.done)
	}
	return nil
}
func (m *restoreSourceMounter) MountBundle(context.Context, int) (nsmount.MountPoint, error) {
	return restoreSourceMount{done: m.bundleDone, name: "bundle", events: m.events}, nil
}
func (m *restoreSourceMounter) MountCuInterpose(context.Context, nsmount.MountPoint) (nsmount.MountPoint, error) {
	return restoreSourceMount{name: "snapshot-cuda", events: m.events}, nil
}
func (m *restoreSourceMounter) source(name string) (nsmount.MountPoint, error) {
	*m.events = append(*m.events, "mount "+name)
	if m.cancel != nil {
		m.cancel()
	}
	if m.fail {
		return nil, errors.New("mount failed")
	}
	return restoreSourceMount{name: name, events: m.events}, nil
}
func (m *restoreSourceMounter) MountArtifact(_ context.Context, _ nsmount.MountPoint, path string) (nsmount.MountPoint, error) {
	m.artifact = path
	return m.source("artifact")
}
func (m *restoreSourceMounter) MountPageBroker(_ context.Context, _ nsmount.MountPoint, path string) (nsmount.MountPoint, error) {
	m.staged = path
	return m.source("staged")
}

func TestRestoreSourceAndAbortCleanup(t *testing.T) {
	for _, tc := range []struct {
		name         string
		mode         string
		mountFailure bool
		cancel       bool
	}{
		{name: "default direct"},
		{name: "explicit staged", mode: "staged"},
		{name: "direct", mode: "direct"},
		{name: "direct mount failure", mode: "direct", mountFailure: true},
		{name: "direct cancellation", mode: "direct", mountFailure: true, cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			artifact, err := nsmount.ResolveArtifactPath(base, "content", "main")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(artifact, 0700); err != nil {
				t.Fatal(err)
			}
			manifest := &types.CheckpointManifest{Artifact: types.ArtifactManifest{ContentUID: "content", ContainerName: "main"}}
			if err := types.WriteManifest(artifact, manifest); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "broker.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var events []string
			bundleDone := make(chan struct{})
			mounts := &restoreSourceMounter{fail: tc.mountFailure, events: &events, bundleDone: bundleDone}
			if tc.cancel {
				mounts.cancel = cancel
			}
			server := make(chan error, 1)
			go func() {
				transaction := ""
				for i := 0; i < 2; i++ {
					connection, err := listener.Accept()
					if err != nil {
						server <- err
						return
					}
					err = func() error {
						defer connection.Close()
						if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
							return err
						}
						var size uint32
						if err := binary.Read(connection, binary.BigEndian, &size); err != nil {
							return err
						}
						data := make([]byte, size)
						if _, err := io.ReadFull(connection, data); err != nil {
							return err
						}
						request := new(pagebroker.Request)
						if err := proto.Unmarshal(data, request); err != nil {
							return err
						}
						response := &pagebroker.Response{RequestId: request.RequestId, TransactionId: request.TransactionId}
						if i == 0 {
							transaction = request.GetTransactionId()
							if transaction == "" {
								return errors.New("missing transaction")
							}
							if tc.mode != "staged" {
								if request.GetDirectRestore().GetSource().GetFilesystem().GetDirectory() != artifact {
									return errors.New("wrong direct restore source")
								}
								response.Result = &pagebroker.Response_DirectRestoreReady{DirectRestoreReady: &pagebroker.DirectRestoreReady{}}
							} else {
								if request.GetStagedRestore().GetSource().GetFilesystem().GetDirectory() != artifact {
									return errors.New("wrong staged restore source")
								}
								response.Result = &pagebroker.Response_StagedRestoreDirectory{StagedRestoreDirectory: &pagebroker.StagedRestoreDirectory{ImageDirectory: proto.String("/pagebroker/staging/restore/test")}}
							}
						} else {
							if request.GetAbort() == nil || request.GetTransactionId() != transaction {
								return errors.New("expected abort of prepared restore")
							}
							select {
							case <-bundleDone:
							default:
								return errors.New("abort arrived before mount cleanup")
							}
							response.Result = &pagebroker.Response_AbortComplete{AbortComplete: &pagebroker.AbortComplete{}}
						}
						data, err = proto.Marshal(response)
						if err != nil {
							return err
						}
						if err := binary.Write(connection, binary.BigEndian, uint32(len(data))); err != nil {
							return err
						}
						_, err = connection.Write(data)
						return err
					}()
					if err != nil {
						server <- err
						return
					}
				}
				server <- nil
			}()
			_, err = Restore(ctx, &restoreFakeRuntime{}, testr.New(t), RestoreRequest{
				BasePath: base, ContentUID: "content", ArtifactContainerName: "main", ContainerID: "placeholder",
				SkipCompatCheck: true, PageBrokerEnabled: true, PageBrokerRequested: true,
				PageBrokerRestoreMode: tc.mode, PageBrokerControlSocketPath: listener.Addr().String(),
			}, mounts)
			// The fixture has no namespace FD, so nsrestore cannot launch.
			if err == nil {
				t.Fatal("restore unexpectedly succeeded")
			}
			select {
			case err := <-server:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(6 * time.Second):
				t.Fatal("restore did not abort its transaction")
			}
			name := "staged"
			if tc.mode != "staged" {
				name = "artifact"
				if mounts.artifact != artifact || mounts.staged != "" {
					t.Fatalf("wrong source mount: %+v", mounts)
				}
			} else if mounts.staged != "/pagebroker/staging/restore/test" || mounts.artifact != "" {
				t.Fatalf("wrong staging mount: %+v", mounts)
			}
			want := []string{"mount " + name}
			if !tc.mountFailure {
				want = append(want, "unmount "+name)
			}
			want = append(want, "unmount bundle")
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("cleanup = %v, want %v", events, want)
			}
			if _, err := types.ReadManifest(artifact); err != nil {
				t.Fatalf("source was removed: %v", err)
			}
		})
	}
}

func testCuInterposeIdentity() *types.CuInterposeManifest {
	return &types.CuInterposeManifest{
		Libraries: map[string]types.CuInterposeLibraryIdentity{
			types.CuInterposeFrontend: {SHA256: strings.Repeat("a", 64)},
			types.CuInterposeCore:     {SHA256: strings.Repeat("b", 64)},
		},
		PIDs: []int{},
	}
}

type cleanupMount struct {
	count  *int
	events chan<- string
	name   string
}

func (m cleanupMount) Unmount(context.Context) error {
	*m.count++
	m.events <- "unmount " + m.name
	return nil
}

func (m cleanupMount) NsFd() *os.File {
	return nil
}

func (m cleanupMount) Release() error { return nil }

type cleanupMounter struct {
	bundle, staging, mounted int
	events                   chan<- string
}

func (m *cleanupMounter) MountBundle(context.Context, int) (nsmount.MountPoint, error) {
	m.mounted++
	return cleanupMount{&m.bundle, m.events, "bundle"}, nil
}
func (m *cleanupMounter) MountCuInterpose(context.Context, nsmount.MountPoint) (nsmount.MountPoint, error) {
	return nil, errors.New("unexpected cuinterpose mount")
}
func (m *cleanupMounter) MountPageBroker(context.Context, nsmount.MountPoint, string) (nsmount.MountPoint, error) {
	m.mounted++
	return cleanupMount{&m.staging, m.events, "staging"}, nil
}
func (m *cleanupMounter) MountArtifact(context.Context, nsmount.MountPoint, string) (nsmount.MountPoint, error) {
	return nil, errors.New("unexpected artifact mount")
}

func TestStagedRestoreFailureOrdersMountCleanupAndAbort(t *testing.T) {
	const gpuUUID = "GPU-11111111-1111-1111-1111-111111111111"
	for _, mode := range []string{"cpu", "invalid-context", "custom-storage"} {
		t.Run(mode, func(t *testing.T) {
			base := t.TempDir()
			directory, err := nsmount.ResolveArtifactPath(base, "content", "main")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			manifest := types.NewCheckpointManifest("content", "main", types.CRIUDumpManifest{},
				types.SourcePodManifest{}, types.OverlayManifest{}, types.HostManifest{})
			manifest.CUDA.CustomStorage = mode != "cpu"
			runtime := &restoreFakeRuntime{}
			if mode == "custom-storage" {
				manifest.CUDA.PIDs = []int{12}
				manifest.CUDA.SourceGPUUUIDs = []string{gpuUUID}
				runtime.env = []string{"NVIDIA_VISIBLE_DEVICES=" + gpuUUID}
				tools := t.TempDir()
				if err := os.WriteFile(filepath.Join(tools, "nvidia-smi"), []byte("#!/bin/sh\necho '"+gpuUUID+", Test GPU, 590.00'\n"), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", tools)
			}
			if err := types.WriteManifest(directory, manifest); err != nil {
				t.Fatal(err)
			}
			listener := listenPageBroker(t)
			server := make(chan error, 1)
			// Leave room for duplicate cleanup events so the order assertion reports them.
			events := make(chan string, 8)
			go func() {
				steps := []string{"stage", "abort"}
				if mode == "invalid-context" {
					steps = nil
				}
				for _, step := range steps {
					connection, err := listener.Accept()
					if err != nil {
						server <- err
						return
					}
					request, err := readPageBrokerTestRequest(connection)
					if err != nil {
						connection.Close()
						server <- err
						return
					}
					response := &pagebroker.Response{}
					switch step {
					case "stage":
						if request.GetStagedRestore() == nil {
							err = errors.New("expected StagedRestore")
						}
						response.Result = &pagebroker.Response_StagedRestoreDirectory{StagedRestoreDirectory: &pagebroker.StagedRestoreDirectory{ImageDirectory: &directory}}
					case "abort":
						if request.GetAbort() == nil {
							err = errors.New("expected Abort")
						}
						events <- "abort"
						response.Result = &pagebroker.Response_AbortComplete{AbortComplete: &pagebroker.AbortComplete{}}
					}
					if err == nil {
						err = replyPageBrokerTest(connection, request, response)
					}
					connection.Close()
					if err != nil {
						server <- err
						return
					}
				}
				server <- nil
			}()
			mounts := &cleanupMounter{events: events}
			ctx, cancel := context.WithTimeout(context.Background(), pageBrokerTestTimeout)
			defer cancel()
			_, err = Restore(ctx, runtime, testr.New(t), RestoreRequest{
				BasePath: base, ContentUID: "content", ArtifactContainerName: "main", ContainerID: "placeholder",
				PageBrokerEnabled: true, PageBrokerRequested: true, PageBrokerControlSocketPath: listener.Addr().String(),
				PageBrokerRestoreMode: "staged", CustomStorageAvailable: true, SkipCompatCheck: true,
			}, mounts)
			if err == nil {
				t.Fatal("expected restore failure")
			}
			wantCleanup := 0
			if mode != "invalid-context" {
				wantCleanup = 1
			}
			if mode == "invalid-context" && !strings.Contains(err.Error(), "GPU context requires captured PIDs") {
				t.Fatalf("expected GPU context failure before staging: %v", err)
			}
			if mode == "custom-storage" && !strings.Contains(err.Error(), "nsrestore failed") {
				t.Fatalf("expected failure after staging mount: %v", err)
			}
			if mounts.mounted != 2*wantCleanup || mounts.bundle != wantCleanup || mounts.staging != wantCleanup {
				t.Fatalf("mount cleanup: %+v; restore: %v", mounts, err)
			}
			waitPageBrokerTest(t, server)
			close(events)
			var observed []string
			for event := range events {
				observed = append(observed, event)
			}
			var want []string
			switch mode {
			case "cpu":
				want = []string{"unmount staging", "unmount bundle", "abort"}
			case "custom-storage":
				want = []string{"abort", "unmount staging", "unmount bundle"}
			}
			if !reflect.DeepEqual(observed, want) {
				t.Fatalf("cleanup order = %v, want %v", observed, want)
			}
		})
	}
}

func TestFailedRestoreWaitsForAbortBeforeTermination(t *testing.T) {
	for _, outcome := range []string{"drained", "termination-error"} {
		t.Run(outcome, func(t *testing.T) {
			listener := listenPageBroker(t)
			gpu, err := (pagebroker.Client{ControlSocketPath: listener.Addr().String()}).OpenCustomStorageExecution("failed-restore", &pagebroker.GpuContext{})
			if err != nil {
				t.Fatal(err)
			}
			defer gpu.Close()
			// The parent's endpoint was connected by nsrestore before it exited.
			if err := unix.Connect(int(gpu.Socket.Fd()), &unix.SockaddrUnix{Name: listener.Addr().String()}); err != nil {
				t.Fatal(err)
			}
			peer, err := listener.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			abortReceived, replyAllowed := make(chan struct{}), make(chan struct{})
			server := make(chan error, 1)
			go func() {
				connection, err := listener.Accept()
				if err != nil {
					server <- err
					return
				}
				defer connection.Close()
				request, err := readPageBrokerTestRequest(connection)
				if err != nil || request.GetAbort() == nil || request.GetTransactionId() != "failed-restore" {
					server <- fmt.Errorf("restore Abort: %v, %v", request, err)
					return
				}
				close(abortReceived)
				select {
				case <-replyAllowed:
				case <-time.After(pageBrokerTestTimeout):
					server <- errors.New("test did not release Abort reply")
					return
				}
				server <- replyPageBrokerTest(connection, request, &pagebroker.Response{
					Result: &pagebroker.Response_AbortComplete{AbortComplete: &pagebroker.AbortComplete{}},
				})
			}()
			terminated := make(chan struct{}, 1)
			finished := make(chan error, 1)
			stopError := errors.New("container termination failed")
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			go func() {
				finished <- abortRestoreTransaction(ctx, gpu, func(context.Context) error {
					terminated <- struct{}{}
					if outcome == "termination-error" {
						return stopError
					}
					return nil
				})
			}()
			select {
			case <-abortReceived:
			case err := <-server:
				t.Fatalf("Abort request failed: %v", err)
			case <-time.After(pageBrokerTestTimeout):
				t.Fatal("Abort did not arrive")
			}
			select {
			case <-terminated:
				t.Fatal("terminated before broker confirmed drain")
			case err := <-finished:
				t.Fatalf("cleanup returned before drain: %v", err)
			default:
			}
			close(replyAllowed)
			select {
			case err := <-finished:
				if outcome == "termination-error" {
					if !errors.Is(err, stopError) {
						t.Fatalf("termination error was lost: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			case <-time.After(pageBrokerTestTimeout):
				t.Fatal("cleanup did not finish after drain")
			}
			if len(terminated) != 1 {
				t.Fatal("failed container was not terminated exactly once")
			}
			waitPageBrokerTest(t, server)
		})
	}
}

func TestCustomStorageFilesReachChild(t *testing.T) {
	if os.Getenv("SNAPSHOT_TEST_CUSTOM_STORAGE_CHILD") == "wait-cancel" {
		file := os.NewFile(3, "cancellation")
		defer file.Close()
		var data [1]byte
		if _, err := file.Read(data[:]); !errors.Is(err, io.EOF) {
			t.Fatalf("cancellation pipe: %v", err)
		}
		_, _ = os.Stdout.WriteString("child observed cancellation\n")
		return
	}
	if os.Getenv("SNAPSHOT_TEST_CUSTOM_STORAGE_CHILD") == "1" {
		values := make(map[string]string)
		for i := 0; i < len(os.Args)-1; i++ {
			if strings.HasPrefix(os.Args[i], "--") {
				values[os.Args[i]] = os.Args[i+1]
			}
		}
		if values["--pagebroker-transaction"] != "restore" || values["--pagebroker-socket-name"] != "broker.sock" {
			t.Fatal("CustomStorage flags did not reach child")
		}
		for flag, want := range map[string]int{
			"--pagebroker-socket-directory-fd": firstExtraFileDescriptor + 2,
			"--host-proc-fd":                   firstExtraFileDescriptor + 3,
		} {
			fd, err := strconv.Atoi(values[flag])
			if err != nil || fd != want {
				t.Fatalf("%s=%q, want %d", flag, values[flag], want)
			}
			file := os.NewFile(uintptr(fd), flag)
			defer file.Close()
			data, err := os.ReadFile(fmt.Sprintf("/proc/self/fd/%d/marker", fd))
			if err != nil || string(data) != flag {
				t.Fatalf("inherited %s: %q, %v", flag, data, err)
			}
		}
		for flag, kind := range map[string]uint32{
			"--pagebroker-execution-fd": unix.S_IFSOCK,
			"--cancel-fd":               unix.S_IFIFO,
		} {
			fd, err := strconv.Atoi(values[flag])
			if err != nil {
				t.Fatal(err)
			}
			var info unix.Stat_t
			if err := unix.Fstat(fd, &info); err != nil || info.Mode&unix.S_IFMT != kind {
				t.Fatalf("inherited %s has wrong type: %v", flag, err)
			}
		}
		gpuContext := new(pagebroker.GpuContext)
		if err := json.Unmarshal([]byte(values["--gpu-context"]), gpuContext); err != nil || len(gpuContext.CapturedPids) != 1 || gpuContext.CapturedPids[0] != 12 {
			t.Fatalf("inherited GPU context: %v, %v", gpuContext, err)
		}
		cancelFD, err := strconv.Atoi(values["--cancel-fd"])
		if err != nil {
			t.Fatal(err)
		}
		// Model nsenter exiting while nsrestore still owns the stdout pipe.
		child := exec.Command(os.Args[0], "-test.run=^TestCustomStorageFilesReachChild$")
		child.Env = append(os.Environ(), "SNAPSHOT_TEST_CUSTOM_STORAGE_CHILD=wait-cancel")
		child.ExtraFiles = []*os.File{os.NewFile(uintptr(cancelFD), "cancellation")}
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		return
	}
	openDirectory := func(marker string) *os.File {
		directory := t.TempDir()
		if err := os.WriteFile(filepath.Join(directory, "marker"), []byte(marker), 0600); err != nil {
			t.Fatal(err)
		}
		file, err := os.Open(directory)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = file.Close() })
		return file
	}
	socket, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	executionSocket := os.NewFile(uintptr(socket), "execution-socket")
	defer executionSocket.Close()
	execution := &pagebroker.CustomStorageExecution{
		Socket:          executionSocket,
		SocketDirectory: openDirectory("--pagebroker-socket-directory-fd"),
		SocketName:      "broker.sock",
		GPUContext:      &pagebroker.GpuContext{CapturedPids: []uint32{12}},
		TransactionID:   "restore",
	}
	hostProc := openDirectory("--host-proc-fd")
	ctx, cancel := context.WithTimeout(context.Background(), pageBrokerTestTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCustomStorageFilesReachChild$", "--")
	cmd.Env = append(os.Environ(), "SNAPSHOT_TEST_CUSTOM_STORAGE_CHILD=1")
	cmd.ExtraFiles = []*os.File{openDirectory("mount-namespace"), openDirectory("nsrestore-binary")}
	closeFiles, err := addCustomStorageFiles(ctx, cmd, execution, hostProc)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFiles()
	// After the wrapper exits, Cmd's process watcher no longer cancels it.
	// Disable that path so this test requires the independent context callback.
	cmd.Cancel = nil
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	wrapperPidfd := -1
	cmd.SysProcAttr = &syscall.SysProcAttr{PidFD: &wrapperPidfd}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(wrapperPidfd)
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
	poll := []unix.PollFd{{Fd: int32(wrapperPidfd), Events: unix.POLLIN}}
	if n, err := unix.Poll(poll, int(pageBrokerTestTimeout/time.Millisecond)); err != nil || n != 1 || poll[0].Revents&unix.POLLIN == 0 {
		t.Fatalf("wrapper did not exit: %v, %v", poll, err)
	}
	select {
	case err := <-finished:
		t.Fatalf("child did not retain stdout after wrapper exit: %v\n%s", err, output.String())
	default:
	}
	cancel()
	select {
	case err := <-finished:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("child descriptor handoff: %v\n%s", err, output.String())
		}
		if !strings.Contains(output.String(), "child observed cancellation") {
			t.Fatalf("cancellation did not reach surviving child: %s", output.String())
		}
	case <-time.After(pageBrokerTestTimeout):
		t.Fatal("wrapper exit stopped cancellation of surviving child")
	}
}

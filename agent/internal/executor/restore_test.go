// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/ai-dynamo/snapshot/agent/internal/criu"
	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/compat"
	"github.com/ai-dynamo/snapshot/api/podcontract"
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
					}, nsmount.New(log))
					// CustomStorage checks broker support before runtime inspection.
					want := "stop after path preparation"
					if storage == "custom" && engine {
						want = "check PageBroker CustomStorage support"
					}
					if storage == "custom" && engine && !requested {
						want = "CustomStorage restore requires nvidia.com/snapshot-pagebroker=true"
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
	if err == nil || !strings.Contains(err.Error(), "CustomStorage restore requires PageBroker GPU execution") {
		t.Fatalf("unexpected missing-session result: %v", err)
	}
}

func TestCUDAHelperLibraryDirectorySurvivesBundleRemoval(t *testing.T) {
	bundle := t.TempDir()
	libraryDir := filepath.Join(bundle, "lib")
	if err := os.Mkdir(libraryDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(libraryDir, "libfixture.so"), []byte("library"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LD_LIBRARY_PATH", "/old-libraries")
	libraries, err := os.Open(libraryDir)
	if err != nil {
		t.Fatal(err)
	}
	defer libraries.Close()
	if err := os.Rename(libraryDir, filepath.Join(bundle, "detached")); err != nil {
		t.Fatal(err)
	}
	restoreLibraryPath, err := useCUDAHelperLibraries(libraries)
	if err != nil {
		t.Fatal(err)
	}
	defer restoreLibraryPath()
	path, _, _ := strings.Cut(os.Getenv("LD_LIBRARY_PATH"), ":")
	data, err := os.ReadFile(filepath.Join(path, "libfixture.so"))
	if err != nil || string(data) != "library" {
		t.Fatalf("pinned library lookup failed after original path disappeared: %q, %v", data, err)
	}
	restoreLibraryPath()
	if err := libraries.Close(); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LD_LIBRARY_PATH") != "/old-libraries" {
		t.Fatal("library environment was not restored")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("library descriptor remains after cleanup: %v", err)
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
			err := validateRestoreManifest(tc.req, manifest, t.TempDir())
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
		name         string
		cuInterpose  bool
		frontendOnly bool
		jobFile      string
		wantStopsAt  string
	}{
		{name: "native multi-GPU missing", wantStopsAt: "missing CUDA launch-job state"},
		{name: "native multi-GPU present", jobFile: "present", wantStopsAt: "invalid target pod IP"},
		{name: "frontend-only multi-GPU missing", cuInterpose: true, frontendOnly: true, wantStopsAt: "missing CUDA launch-job state"},
		{name: "frontend-only multi-GPU present", cuInterpose: true, frontendOnly: true, jobFile: "present", wantStopsAt: "invalid target pod IP"},
		{name: "cuinterpose missing",
			cuInterpose: true,
			wantStopsAt: "invalid target pod IP"},
		{name: "cuinterpose present", jobFile: "present",
			cuInterpose: true,
			wantStopsAt: "invalid target pod IP"},
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
			if tc.cuInterpose {
				manifest.CuInterpose = testCuInterposeIdentity()
				if !tc.frontendOnly {
					manifest.CuInterpose.PIDs = []int{43}
				}
			}
			// Stopping at IP validation exercises jobfile selection in the real restore
			// preflight without reaching namespace or CUDA operations.
			manifest.CRIUDump.CRIU.TcpEstablished = true
			t.Setenv(criu.InetRemapEnvVar, "")
			if err := types.WriteManifest(checkpointDir, manifest); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(checkpointDir, podcontract.CUDAJobFileName)
			if tc.jobFile == "present" {
				if err := os.WriteFile(path, []byte("job-state"), 0600); err != nil {
					t.Fatal(err)
				}
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

type cleanupMount struct{ count *int }

func (m cleanupMount) Unmount(context.Context) error { *m.count++; return nil }
func (m cleanupMount) NsFd() *os.File                { return nil }

type cleanupMounter struct{ bundle, staging int }

func (m *cleanupMounter) MountBundle(context.Context, int) (nsmount.MountPoint, error) {
	return cleanupMount{&m.bundle}, nil
}
func (m *cleanupMounter) MountPageBroker(context.Context, nsmount.MountPoint, string) (nsmount.MountPoint, error) {
	return cleanupMount{&m.staging}, nil
}
func (m *cleanupMounter) MountArtifact(context.Context, nsmount.MountPoint, string) (nsmount.MountPoint, error) {
	return nil, errors.New("unexpected artifact mount")
}

func TestCPUStagedRestoreCleansMountsWhenAbortReplyIsLost(t *testing.T) {
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
	if err := types.WriteManifest(directory, manifest); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "broker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := make(chan error, 1)
	go func() {
		for stage := 0; stage < 2; stage++ {
			conn, err := listener.Accept()
			if err != nil {
				server <- err
				return
			}
			request := new(pagebroker.Request)
			var size uint32
			err = binary.Read(conn, binary.BigEndian, &size)
			if err == nil {
				data := make([]byte, size)
				_, err = io.ReadFull(conn, data)
				if err == nil {
					err = proto.Unmarshal(data, request)
				}
			}
			if err != nil {
				conn.Close()
				server <- err
				return
			}
			if stage == 1 {
				conn.Close()
				if request.GetAbort() == nil {
					server <- errors.New("expected Abort")
					return
				}
				server <- nil
				return
			}
			response := &pagebroker.Response{RequestId: request.RequestId, TransactionId: request.TransactionId,
				Result: &pagebroker.Response_StagedRestoreDirectory{StagedRestoreDirectory: &pagebroker.StagedRestoreDirectory{ImageDirectory: &directory}}}
			data, err := proto.Marshal(response)
			if err == nil {
				err = binary.Write(conn, binary.BigEndian, uint32(len(data)))
			}
			if err == nil {
				_, err = conn.Write(data)
			}
			conn.Close()
			if err != nil {
				server <- err
				return
			}
		}
	}()
	mounts := &cleanupMounter{}
	_, err = Restore(context.Background(), &restoreFakeRuntime{}, testr.New(t), RestoreRequest{
		BasePath: base, ContentUID: "content", ArtifactContainerName: "main", ContainerID: "placeholder",
		PageBrokerEnabled: true, PageBrokerRequested: true, PageBrokerControlSocketPath: listener.Addr().String(),
	}, mounts)
	if err == nil {
		t.Fatal("expected restore failure")
	}
	if mounts.bundle != 1 || mounts.staging != 1 {
		t.Fatalf("mount cleanup: %+v; restore: %v", mounts, err)
	}
	select {
	case err := <-server:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Abort was not sent")
	}
}

func testCuInterposeIdentity() *types.CuInterposeManifest {
	return &types.CuInterposeManifest{FrontendSHA256: strings.Repeat("a", 64), CoreSHA256: strings.Repeat("b", 64), PIDs: []int{}}
}

func TestRestoreManifestRequiresMatchingShimLibrariesEvenWhenCompatibilityIsSkipped(t *testing.T) {
	libraries := []struct{ name, contents string }{
		{"libcuinterpose.so", "frontend"},
		{"libcuinterpose_core.so", "core"},
	}
	manifest := &types.CheckpointManifest{
		Artifact: types.ArtifactManifest{ContentUID: "content", ContainerName: "main"},
		CuInterpose: &types.CuInterposeManifest{
			FrontendSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(libraries[0].contents))),
			CoreSHA256:     fmt.Sprintf("%x", sha256.Sum256([]byte(libraries[1].contents))),
			PIDs:           []int{},
		},
	}
	for _, tc := range []struct{ name, missing, changed string }{
		{name: "matching libraries"},
		{name: "missing frontend", missing: libraries[0].name},
		{name: "missing core", missing: libraries[1].name},
		{name: "different frontend", changed: libraries[0].name},
		{name: "different core", changed: libraries[1].name},
	} {
		for _, skip := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/skip-compat=%t", tc.name, skip), func(t *testing.T) {
				bundle := t.TempDir()
				var wantExpected, wantActual string
				for _, library := range libraries {
					if library.name == tc.missing {
						continue
					}
					contents := []byte(library.contents)
					if library.name == tc.changed {
						wantExpected = fmt.Sprintf("%x", sha256.Sum256(contents))
						contents[0] ^= 1 // Preserve size so only identity distinguishes them.
						wantActual = fmt.Sprintf("%x", sha256.Sum256(contents))
					}
					require.NoError(t, os.WriteFile(filepath.Join(bundle, library.name), contents, 0600))
				}
				err := validateRestoreManifest(RestoreRequest{
					ContentUID: "content", ArtifactContainerName: "main", SkipCompatCheck: skip,
				}, manifest, bundle)
				switch {
				case tc.missing != "":
					require.ErrorIs(t, err, os.ErrNotExist)
					require.ErrorContains(t, err, "open restore library "+tc.missing)
				case tc.changed != "":
					require.ErrorContains(t, err, tc.changed+" SHA-256 mismatch")
					require.ErrorContains(t, err, "expected "+wantExpected+", actual "+wantActual)
				default:
					require.NoError(t, err)
				}
			})
		}
	}
}

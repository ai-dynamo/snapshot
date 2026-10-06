// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
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

	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/compat"
	"google.golang.org/protobuf/proto"
)

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

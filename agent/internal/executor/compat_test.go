// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/testr"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/require"

	"github.com/ai-dynamo/snapshot/agent/internal/criu"
	"github.com/ai-dynamo/snapshot/agent/internal/cuda"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/compat"
)

const (
	wholeGPUListing = `printf '%s\n' 'GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-5c89852c-d268-c3f3-1b07-005d5ae1dc3f)'`
	migSliceListing = wholeGPUListing + ` '  MIG 1g.5gb      Device  0: (UUID: MIG-a)'`
)

// installFakeMIGListing answers the nvidia-smi -L probe for pid 42 with body,
// and fails it when called any other way.
func installFakeMIGListing(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nset -eu\n" +
		`test "$*" = "--mount=/host/proc/42/ns/mnt --pid=/host/proc/42/ns/pid -- nvidia-smi -L"` + "\n" +
		body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "nsenter"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake nsenter: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRefuseMIGCapture(t *testing.T) {
	tests := []struct {
		name    string
		listing string
		want    string
	}{
		{
			name:    "a slice is refused",
			listing: migSliceListing + ` '  MIG 1g.5gb      Device  1: (UUID: MIG-b)'`,
			want:    "refused as incompatible: mig-slice: the container holds MIG slice 1g.5gb MIG-a, 1g.5gb MIG-b, and Snapshot supports whole GPUs only",
		},
		{name: "a whole GPU is captured", listing: wholeGPUListing},
		{name: "slices that cannot be listed are captured", listing: "exit 17"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			installFakeMIGListing(t, tc.listing)
			err := refuseMIGCapture(logr.Discard(), 42, cuda.ListVisibleGPUs(context.Background(), "/host/proc", 42, nil))
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, tc.want)
		})
	}
}

func TestRefuseMIGRestore(t *testing.T) {
	tests := []struct {
		name    string
		listing string
		skip    bool
		want    string
	}{
		{
			name:    "a slice is refused at the inspect gate",
			listing: migSliceListing,
			want:    "restore refused as incompatible: mig-slice: source whole GPU, target MIG slice 1g.5gb MIG-a",
		},
		{name: "a whole GPU is admitted", listing: wholeGPUListing},
		{name: "slices that cannot be listed are admitted", listing: "exit 17"},
		{name: "a skipped check admits a slice", listing: migSliceListing, skip: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			installFakeMIGListing(t, tc.listing)
			err := refuseMIGRestore(testr.New(t), 42, cuda.ListVisibleGPUs(context.Background(), "/host/proc", 42, nil), tc.skip)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refuseMIGRestore() = %v, want nil", err)
				}
				return
			}
			var incompatible *compat.IncompatibleError
			if !errors.As(err, &incompatible) || incompatible.Gate != compat.GateInspect {
				t.Fatalf("refuseMIGRestore() = %v, want an inspect gate refusal", err)
			}
			if err.Error() != tc.want {
				t.Fatalf("refuseMIGRestore() = %q, want %q", err, tc.want)
			}
		})
	}
}

// deviceRulesRuntime resolves a container whose OCI spec carries the device
// cgroup rules a container runtime writes for a container without a GPU.
type deviceRulesRuntime struct {
	restoreFakeRuntime
}

func (r *deviceRulesRuntime) ResolveContainer(context.Context, string) (int, *specs.Spec, error) {
	return 123, &specs.Spec{
		Process: &specs.Process{},
		Linux: &specs.Linux{Resources: &specs.LinuxResources{
			Devices: []specs.LinuxDeviceCgroup{{Allow: false, Access: "rwm"}},
		}},
	}, nil
}

// A restore Pod that asks for no GPU gets no GPU device node, no nvidia-smi and
// none of the files the NVIDIA runtime mounts beside a GPU. The refusal names
// the GPU count, not nsenter's exit status and not each of those files.
func TestInspectRestoreRefusesContainerWithoutGPU(t *testing.T) {
	hostProc := t.TempDir()
	root := filepath.Join(hostProc, "123", "root")
	card := filepath.Join(root, "proc", "driver", "nvidia", "gpus", "0000:00:1e.0")
	require.NoError(t, os.MkdirAll(card, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "dev"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(card, "information"),
		[]byte("Model: \t\t NVIDIA A10G\nGPU UUID: \t GPU-node\nDevice Minor: \t 0\n"), 0o644))
	originalHostProc := hostProcPath
	t.Cleanup(func() { hostProcPath = originalHostProc })
	hostProcPath = hostProc

	tools := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tools, "nsenter"), []byte("#!/bin/sh\nexit 127\n"), 0o755))
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))

	manifest := types.NewCheckpointManifest(
		"content-uid-123",
		"main",
		types.CRIUDumpManifest{ExtMnt: map[string]string{"/usr/bin/nvidia-smi": "/usr/bin/nvidia-smi"}},
		types.NewSourcePodManifest("source-id", 456, "node-1", "source-pod", "default", "10.0.0.11", nil),
		types.OverlayManifest{},
		types.HostManifest{},
	)
	manifest.CUDA = types.CUDAManifest{PIDs: []int{1}, SourceGPUUUIDs: []string{"GPU-source"}}

	// No Pod name, so discovery asks neither Kubernetes nor kubelet.
	_, _, err := inspectRestore(context.Background(), &deviceRulesRuntime{}, testr.New(t), RestoreRequest{
		ContentUID:               "content-uid-123",
		ContainerID:              "placeholder-id",
		ArtifactContainerName:    "main",
		DestinationContainerName: "main",
		WithoutProxy:             true,
	}, manifest)

	var incompatible *compat.IncompatibleError
	require.ErrorAs(t, err, &incompatible)
	require.Equal(t, compat.GateInspect, incompatible.Gate)
	require.Equal(t, "restore refused as incompatible: gpu-count: source 1, target 0", err.Error())
}

func TestRefuseUnlabeledRestore(t *testing.T) {
	const profile = "apparmor:cri-containerd.apparmor.d"
	labeled := &types.CheckpointManifest{CRIUDump: types.CRIUDumpManifest{LSMLabels: true}}
	unlabeled := &types.CheckpointManifest{}
	tests := []struct {
		name     string
		manifest *types.CheckpointManifest
		profile  string
		want     string
	}{
		{name: "a labeled checkpoint is admitted", manifest: labeled, profile: profile},
		{name: "a target without a label admits any checkpoint", manifest: unlabeled, profile: criu.LSMProfileNone},
		{
			name:     "an unlabeled checkpoint is refused",
			manifest: unlabeled,
			profile:  profile,
			want:     "restore refused as incompatible: lsm-labels: source no recorded process labels, target " + profile,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := refuseUnlabeledRestore(tc.manifest, tc.profile)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refuseUnlabeledRestore() = %v, want nil", err)
				}
				return
			}
			var incompatible *compat.IncompatibleError
			if !errors.As(err, &incompatible) || incompatible.Gate != compat.GateInspect {
				t.Fatalf("refuseUnlabeledRestore() = %v, want an inspect gate refusal", err)
			}
			if err.Error() != tc.want {
				t.Fatalf("refuseUnlabeledRestore() = %q, want %q", err, tc.want)
			}
		})
	}
}

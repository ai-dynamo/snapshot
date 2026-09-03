// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	criurpc "github.com/checkpoint-restore/go-criu/v8/rpc"
	"google.golang.org/protobuf/proto"

	"github.com/ai-dynamo/snapshot/api/compat"
)

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()

	original := NewCheckpointManifest(
		"content-uid-123",
		"main",
		CRIUDumpManifest{
			CRIU: CRIUSettings{
				LogLevel: 4,
				ShellJob: true,
				LibDir:   "/usr/lib/criu",
			},
			ExtMnt:   map[string]string{"/etc/hostname": "/etc/hostname", "/proc/acpi": "/dev/null"},
			External: []string{"net[12345]:extNetNs"},
			SkipMnt:  []string{"/proc/kcore"},
		},
		NewSourcePodManifest("ctr-abc", 42, "node-1", "my-pod", "default", "10.0.0.11", []string{"pipe:[111]", "pipe:[222]", "pipe:[333]"}),
		OverlayManifest{
			Exclusions:     OverlaySettings{Exclusions: []string{"/proc", "/sys"}},
			UpperDir:       "/var/lib/containerd/upper",
			ExternalPaths:  []string{"/proc/acpi"},
			BindMountDests: []string{"/data"},
		},
		NewHostManifest("5.15.0-1071-aws"),
	)
	original.CUDA = NewCUDAManifest([]int{42, 43}, compat.GPUFacts{
		DriverVersion: "580.65.06",
		Devices: []compat.GPUDevice{
			{UUID: "GPU-aaa", ProductName: "NVIDIA A100-SXM4-40GB"},
			{UUID: "GPU-bbb", ProductName: "NVIDIA A100-SXM4-40GB"},
		},
	})

	if err := WriteManifest(dir, original); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	loaded, err := ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}

	// Verify key fields survived the round-trip
	if loaded.Artifact != original.Artifact {
		t.Errorf("Artifact = %#v, want %#v", loaded.Artifact, original.Artifact)
	}
	if loaded.CRIUDump.CRIU.LogLevel != 4 {
		t.Errorf("CRIU.LogLevel = %d, want 4", loaded.CRIUDump.CRIU.LogLevel)
	}
	if loaded.CRIUDump.CRIU.ShellJob != true {
		t.Error("CRIU.ShellJob should be true")
	}
	if len(loaded.CRIUDump.ExtMnt) != 2 {
		t.Errorf("ExtMnt count = %d, want 2", len(loaded.CRIUDump.ExtMnt))
	}
	if loaded.CRIUDump.ExtMnt["/etc/hostname"] != "/etc/hostname" {
		t.Errorf("ExtMnt[/etc/hostname] = %q", loaded.CRIUDump.ExtMnt["/etc/hostname"])
	}
	if len(loaded.CRIUDump.External) != 1 || loaded.CRIUDump.External[0] != "net[12345]:extNetNs" {
		t.Errorf("External = %v", loaded.CRIUDump.External)
	}
	if len(loaded.CRIUDump.SkipMnt) != 1 || loaded.CRIUDump.SkipMnt[0] != "/proc/kcore" {
		t.Errorf("SkipMnt = %v", loaded.CRIUDump.SkipMnt)
	}
	if loaded.K8s.PodName != "my-pod" {
		t.Errorf("K8s.PodName = %q", loaded.K8s.PodName)
	}
	if loaded.K8s.PodIP != "10.0.0.11" {
		t.Errorf("K8s.PodIP = %q", loaded.K8s.PodIP)
	}
	if len(loaded.K8s.StdioFDs) != 3 {
		t.Errorf("StdioFDs count = %d, want 3", len(loaded.K8s.StdioFDs))
	}
	if loaded.Overlay.UpperDir != "/var/lib/containerd/upper" {
		t.Errorf("Overlay.UpperDir = %q", loaded.Overlay.UpperDir)
	}
	if len(loaded.Overlay.BindMountDests) != 1 || loaded.Overlay.BindMountDests[0] != "/data" {
		t.Errorf("Overlay.BindMountDests = %v", loaded.Overlay.BindMountDests)
	}
	if len(loaded.CUDA.PIDs) != 2 || loaded.CUDA.PIDs[0] != 42 {
		t.Errorf("CUDA.PIDs = %v", loaded.CUDA.PIDs)
	}
	if len(loaded.CUDA.SourceGPUUUIDs) != 2 || loaded.CUDA.SourceGPUUUIDs[0] != "GPU-aaa" {
		t.Errorf("CUDA.SourceGPUUUIDs = %v", loaded.CUDA.SourceGPUUUIDs)
	}
	if loaded.CUDA.SourceDriverVersion != "580.65.06" {
		t.Errorf("CUDA.SourceDriverVersion = %q", loaded.CUDA.SourceDriverVersion)
	}
	wantGPUs := []GPUManifest{
		{UUID: "GPU-aaa", ProductName: "NVIDIA A100-SXM4-40GB"},
		{UUID: "GPU-bbb", ProductName: "NVIDIA A100-SXM4-40GB"},
	}
	if !reflect.DeepEqual(loaded.CUDA.SourceGPUs, wantGPUs) {
		t.Errorf("CUDA.SourceGPUs = %#v, want %#v", loaded.CUDA.SourceGPUs, wantGPUs)
	}
	wantHost := HostManifest{
		KernelVersion: "5.15.0-1071-aws",
		CPUArch:       runtime.GOARCH,
	}
	if !reflect.DeepEqual(loaded.Host, wantHost) {
		t.Errorf("Host = %#v, want %#v", loaded.Host, wantHost)
	}
}

func TestSourcePodManifestRecordsTheImageAndItsLimits(t *testing.T) {
	dir := t.TempDir()
	original := &CheckpointManifest{Artifact: ArtifactManifest{ContentUID: "content-uid-123", ContainerName: "main"}}
	original.K8s = NewSourcePodManifest("ctr-abc", 42, "node-1", "my-pod", "default", "10.0.0.11", nil)
	original.K8s.Image = "nvcr.io/nvidia/tritonserver:24.09-py3"
	original.K8s.ImageID = "docker-pullable://nvcr.io/nvidia/tritonserver@sha256:deadbeef"
	original.K8s.CPULimit = "4"
	original.K8s.MemoryLimit = "16Gi"

	if err := WriteManifest(dir, original); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	loaded, err := ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if !reflect.DeepEqual(loaded.K8s, original.K8s) {
		t.Errorf("K8s = %#v, want %#v", loaded.K8s, original.K8s)
	}
}

// Every checkpoint already on disk was written before any of these facts
// existed. Such an artifact has to keep parsing, and the facts it never
// recorded have to come back unknown - the manifest carries no schema version,
// so absent keys are the entire compatibility mechanism.
func TestReadManifestAcceptsAnArtifactWrittenBeforeTheseFacts(t *testing.T) {
	dir := t.TempDir()
	legacy := `artifact:
  contentUID: content-uid-123
  containerName: main
createdAt: 2026-03-31T00:00:00Z
criuDump:
  criu:
    logLevel: 4
  extMnt:
    /etc/hostname: /etc/hostname
k8s:
  containerId: ctr-abc
  pid: 42
  sourceNode: node-1
  podName: my-pod
  podNamespace: default
overlay:
  upperDir: /var/lib/containerd/upper
cudaRestore:
  pids: [42]
  sourceGpuUuids: [GPU-aaa]
`
	if err := os.WriteFile(filepath.Join(dir, manifestFilename), []byte(legacy), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	manifest, err := ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	facts := manifest.CompatFacts()
	if facts.Image != "" || facts.ImageID != "" || facts.CPULimit != "" || facts.MemoryLimit != "" {
		t.Errorf("pod facts = %#v, want unknown", facts)
	}
	if facts.KernelVersion != "" || facts.CPUArch != "" {
		t.Errorf("host facts = %#v, want unknown", facts)
	}

	// What the artifact does record still has to arrive, or the older
	// checkpoints would stop being compared at all.
	wantGPUs := []compat.GPUDevice{{UUID: "GPU-aaa"}}
	if !reflect.DeepEqual(facts.GPUDevices, wantGPUs) {
		t.Errorf("GPU devices = %#v, want %#v", facts.GPUDevices, wantGPUs)
	}
	if !reflect.DeepEqual(facts.ExternalizedMounts, []string{"/etc/hostname"}) {
		t.Errorf("externalized mounts = %#v", facts.ExternalizedMounts)
	}
}

// A host fact the agent could not read has to stay absent in the file, because a
// comparison treats absent as unknown and an empty string as a value.
func TestHostManifestOmitsWhatTheAgentCouldNotRead(t *testing.T) {
	dir := t.TempDir()
	manifest := &CheckpointManifest{Artifact: ArtifactManifest{ContentUID: "content-uid-123", ContainerName: "main"}}
	if err := WriteManifest(dir, manifest); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(dir, manifestFilename))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	for _, key := range []string{"kernelVersion", "cpuArch"} {
		if strings.Contains(string(content), key) {
			t.Errorf("manifest wrote an unknown %s:\n%s", key, content)
		}
	}
}

// The facts are recorded to be compared, so what a manifest carries has to come
// back out as the source side of a comparison, one group at a time.
func TestManifestFactsSurviveIntoTheComparison(t *testing.T) {
	tests := []struct {
		name     string
		manifest *CheckpointManifest
		want     compat.Facts
	}{
		{
			name: "host facts",
			manifest: &CheckpointManifest{
				Host: NewHostManifest("5.15.0-1071-aws"),
			},
			want: compat.Facts{KernelVersion: "5.15.0-1071-aws", CPUArch: runtime.GOARCH},
		},
		{
			name: "pod facts",
			manifest: &CheckpointManifest{K8s: SourcePodManifest{
				Image:       "nvcr.io/nvidia/tritonserver:24.09-py3",
				ImageID:     "docker-pullable://nvcr.io/nvidia/tritonserver@sha256:deadbeef",
				CPULimit:    "4",
				MemoryLimit: "16Gi",
			}},
			want: compat.Facts{
				Image:       "nvcr.io/nvidia/tritonserver:24.09-py3",
				ImageID:     "docker-pullable://nvcr.io/nvidia/tritonserver@sha256:deadbeef",
				CPULimit:    "4",
				MemoryLimit: "16Gi",
			},
		},
		{
			name: "GPU facts",
			manifest: &CheckpointManifest{
				CUDA: NewCUDAManifest([]int{1}, compat.GPUFacts{
					DriverVersion: "580.65.06",
					Devices:       []compat.GPUDevice{{UUID: "GPU-aaa", ProductName: "NVIDIA L4"}},
				}),
			},
			want: compat.Facts{
				DriverVersion: "580.65.06",
				GPUDevices:    []compat.GPUDevice{{UUID: "GPU-aaa", ProductName: "NVIDIA L4"}},
			},
		},
		{
			// An artifact captured before the models were recorded still has to
			// report how many GPUs it used, or the count rule would silently stop
			// applying to every checkpoint taken so far.
			name: "GPU facts recorded as UUIDs alone",
			manifest: &CheckpointManifest{
				CUDA: CUDAManifest{PIDs: []int{1}, SourceGPUUUIDs: []string{"GPU-aaa", "GPU-bbb"}},
			},
			want: compat.Facts{
				GPUDevices: []compat.GPUDevice{{UUID: "GPU-aaa"}, {UUID: "GPU-bbb"}},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.manifest.CompatFacts(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("CompatFacts = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestNewCRIUDumpManifest(t *testing.T) {
	t.Run("nil CriuOpts does not panic", func(t *testing.T) {
		m := NewCRIUDumpManifest(nil, CRIUSettings{LogLevel: 2})
		if m.CRIU.LogLevel != 2 {
			t.Errorf("LogLevel = %d, want 2", m.CRIU.LogLevel)
		}
		if m.ExtMnt != nil {
			t.Errorf("ExtMnt should be nil, got %v", m.ExtMnt)
		}
	})

	t.Run("extracts ExtMnt from protobuf correctly", func(t *testing.T) {
		opts := &criurpc.CriuOpts{
			ExtMnt: []*criurpc.ExtMountMap{
				{Key: proto.String("/etc/hosts"), Val: proto.String("/etc/hosts")},
				{Key: proto.String("/proc/acpi"), Val: proto.String("/dev/null")},
				// nil entry and empty key should be skipped
				nil,
				{Key: proto.String(""), Val: proto.String("ignored")},
			},
			External: []string{"net[1234]:extNetNs"},
			SkipMnt:  []string{"/proc/kcore", "/sys/firmware"},
		}

		m := NewCRIUDumpManifest(opts, CRIUSettings{})
		if len(m.ExtMnt) != 2 {
			t.Fatalf("ExtMnt count = %d, want 2; got %v", len(m.ExtMnt), m.ExtMnt)
		}
		if m.ExtMnt["/etc/hosts"] != "/etc/hosts" {
			t.Errorf("ExtMnt[/etc/hosts] = %q", m.ExtMnt["/etc/hosts"])
		}
		if m.ExtMnt["/proc/acpi"] != "/dev/null" {
			t.Errorf("ExtMnt[/proc/acpi] = %q", m.ExtMnt["/proc/acpi"])
		}
		if len(m.External) != 1 {
			t.Errorf("External = %v", m.External)
		}
		if len(m.SkipMnt) != 2 {
			t.Errorf("SkipMnt = %v", m.SkipMnt)
		}
	})

	t.Run("empty ExtMnt entries results in nil map", func(t *testing.T) {
		opts := &criurpc.CriuOpts{
			ExtMnt: []*criurpc.ExtMountMap{
				nil,
				{Key: proto.String(""), Val: proto.String("x")},
			},
		}
		m := NewCRIUDumpManifest(opts, CRIUSettings{})
		if m.ExtMnt != nil {
			t.Errorf("expected nil ExtMnt when all entries are empty/nil, got %v", m.ExtMnt)
		}
	})
}

func TestWriteManifestRejectsMissingArtifactIdentity(t *testing.T) {
	dir := t.TempDir()

	err := WriteManifest(dir, &CheckpointManifest{})
	if err == nil || err.Error() != "checkpoint manifest is missing artifact.contentUID" {
		t.Fatalf("expected missing artifact identity error, got %v", err)
	}
}

func TestReadManifestRejectsMissingArtifactIdentity(t *testing.T) {
	dir := t.TempDir()

	content := []byte("createdAt: 2026-03-31T00:00:00Z\n")
	if err := os.WriteFile(filepath.Join(dir, manifestFilename), content, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := ReadManifest(dir)
	if err == nil || err.Error() != "checkpoint manifest is missing artifact.contentUID" {
		t.Fatalf("expected missing artifact identity error, got %v", err)
	}
}

func TestManifestRequiresContainerName(t *testing.T) {
	err := WriteManifest(t.TempDir(), &CheckpointManifest{Artifact: ArtifactManifest{ContentUID: "content-uid-123"}})
	if err == nil || err.Error() != "checkpoint manifest is missing artifact.containerName" {
		t.Fatalf("expected missing container name error, got %v", err)
	}
}

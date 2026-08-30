// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"

	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

func recordedManifest() *types.CheckpointManifest {
	return &types.CheckpointManifest{
		Artifact: types.ArtifactManifest{ContentUID: "content-uid-1", ContainerName: "main"},
		Host:     types.HostManifest{KernelVersion: "5.15.0-1071-aws", CPUArch: "amd64"},
		K8s: types.SourcePodManifest{
			SourceNode:  "gpu-node-3",
			Image:       "nvcr.io/nvidia/ai-dynamo/vllm-runtime:0.6.1",
			ImageID:     "sha256:9f2c",
			MemoryLimit: "64Gi",
			CPULimit:    "16",
		},
		CUDA: types.CUDAManifest{
			PIDs:                []int{7},
			SourceDriverVersion: "580.82.07",
			SourceGPUs: []types.GPUManifest{
				{UUID: "GPU-1", ProductName: "NVIDIA A100-SXM4-80GB"},
				{UUID: "GPU-2", ProductName: "NVIDIA A100-SXM4-80GB"},
			},
		},
	}
}

// recordedSource is what recordedManifest publishes, written out rather than
// projected, so the expectation cannot agree with the code by construction.
func recordedSource() *snapshotv1alpha1.CheckpointSource {
	return &snapshotv1alpha1.CheckpointSource{
		Devices: &snapshotv1alpha1.CheckpointSourceDevices{
			Nvidia: &snapshotv1alpha1.NvidiaCheckpointSource{
				DriverVersion: "580.82.07",
				Instances: []snapshotv1alpha1.NvidiaCheckpointSourceInstance{
					{ProductName: "NVIDIA A100-SXM4-80GB"},
					{ProductName: "NVIDIA A100-SXM4-80GB"},
				},
			},
		},
		Node: &snapshotv1alpha1.CheckpointSourceNode{
			Name:          "gpu-node-3",
			Architecture:  "amd64",
			KernelVersion: "5.15.0-1071-aws",
		},
		Pod: &snapshotv1alpha1.CheckpointSourcePod{
			Image:       "nvcr.io/nvidia/ai-dynamo/vllm-runtime:0.6.1",
			ImageDigest: "sha256:9f2c",
			Memory:      "64Gi",
			CPU:         "16",
		},
	}
}

func manifestWith(mutate func(*types.CheckpointManifest)) *types.CheckpointManifest {
	manifest := recordedManifest()
	mutate(manifest)
	return manifest
}

func sourceWith(mutate func(*snapshotv1alpha1.CheckpointSource)) *snapshotv1alpha1.CheckpointSource {
	source := recordedSource()
	mutate(source)
	return source
}

// Each case compares the whole projection, so a fact the agent could not read
// has to be absent rather than published blank, and a block left with nothing
// in it has to be gone rather than empty.
func TestCheckpointSourceFromManifest(t *testing.T) {
	for name, tc := range map[string]struct {
		manifest *types.CheckpointManifest
		want     *snapshotv1alpha1.CheckpointSource
	}{
		"everything the capture recorded": {
			manifest: recordedManifest(),
			want:     recordedSource(),
		},
		"no manifest": {},
		"a manifest carrying only its identity": {
			manifest: &types.CheckpointManifest{Artifact: recordedManifest().Artifact},
		},
		// Runtimes disagree on how to report an image ID, and the published
		// digest is the reduction the gate compares, not the raw string.
		"an image ID the runtime reported as a reference": {
			manifest: manifestWith(func(m *types.CheckpointManifest) {
				m.K8s.ImageID = "nvcr.io/nvidia/ai-dynamo/vllm-runtime@sha256:9f2c"
			}),
			want: recordedSource(),
		},
		"a CPU capture": {
			manifest: manifestWith(func(m *types.CheckpointManifest) { m.CUDA = types.CUDAManifest{} }),
			want:     sourceWith(func(s *snapshotv1alpha1.CheckpointSource) { s.Devices = nil }),
		},
		"a container with no limits": {
			manifest: manifestWith(func(m *types.CheckpointManifest) {
				m.K8s.MemoryLimit = ""
				m.K8s.CPULimit = ""
			}),
			want: sourceWith(func(s *snapshotv1alpha1.CheckpointSource) {
				s.Pod.Memory = ""
				s.Pod.CPU = ""
			}),
		},
		"a host the agent could not read": {
			manifest: manifestWith(func(m *types.CheckpointManifest) {
				m.Host = types.HostManifest{}
				m.K8s.SourceNode = ""
			}),
			want: sourceWith(func(s *snapshotv1alpha1.CheckpointSource) { s.Node = nil }),
		},
		// The count is the instance list, so an artifact that recorded UUIDs
		// before models were recorded still says how many GPUs it had.
		"GPUs recorded before their models were": {
			manifest: manifestWith(func(m *types.CheckpointManifest) {
				m.CUDA.SourceGPUs = nil
				m.CUDA.SourceGPUUUIDs = []string{"GPU-1", "GPU-2", "GPU-3"}
			}),
			want: sourceWith(func(s *snapshotv1alpha1.CheckpointSource) {
				s.Devices.Nvidia.Instances = make([]snapshotv1alpha1.NvidiaCheckpointSourceInstance, 3)
			}),
		},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, checkpointSourceFromManifest(tc.manifest))
		})
	}
}

func TestCheckpointSourceAtPathReadsTheManifestBesideTheArtifact(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, types.WriteManifest(dir, recordedManifest()))

	source, err := checkpointSourceAtPath(dir)
	require.NoError(t, err)
	assert.Equal(t, recordedSource(), source)
}

func TestCheckpointSourceAtPathReportsAnUnreadableManifest(t *testing.T) {
	source, err := checkpointSourceAtPath(t.TempDir())
	require.Error(t, err)
	assert.Nil(t, source)
}

// committedArtifact writes a manifest carrying facts where the agent will look
// for the artifact of one work order.
func committedArtifact(t *testing.T, w *NodeController, contentUID string) string {
	t.Helper()

	path, err := nsmount.ResolveArtifactPath(w.config.Storage.BasePath, contentUID, "main")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(path, 0o755))

	manifest := recordedManifest()
	manifest.Artifact = types.ArtifactManifest{ContentUID: contentUID, ContainerName: "main"}
	require.NoError(t, types.WriteManifest(path, manifest))
	return path
}

func assertPublishedTheRecordedSource(t *testing.T, content *snapshotv1alpha1.PodSnapshotContent) {
	t.Helper()

	require.NotNil(t, meta.FindStatusCondition(content.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady))
	assert.Equal(t, recordedSource(), content.Status.Source)
}

func TestMarkCheckpointReadyPublishesWhatTheCaptureRecorded(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	w := makeNodeController(t, &fakeCheckpointer{}, content)
	path := committedArtifact(t, w, string(content.UID))

	require.NoError(t, w.markCheckpointReady(context.Background(), content, path))

	assertPublishedTheRecordedSource(t, getContent(t, w, content.Name))
}

// The artifact is already committed by the time this runs, so losing the facts
// must not cost the capture its Ready.
func TestMarkCheckpointReadyGoesReadyWhenTheManifestCannotBeRead(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	w := makeNodeController(t, &fakeCheckpointer{}, content)

	require.NoError(t, w.markCheckpointReady(context.Background(), content, t.TempDir()))

	got := getContent(t, w, content.Name)
	assert.NotNil(t, meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady))
	assert.Nil(t, got.Status.Source)
}

func TestFreshCapturePublishesTheSourceItRecorded(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	pod := makeSourcePod()
	w := makeNodeController(t, &fakeCheckpointer{}, content, pod)
	w.runtime = &fakeRuntime{resolveContainerPID: 7}
	w.checkpointFn = func(_ context.Context, params CheckpointParams) error {
		committedArtifact(t, w, params.ContentUID)
		return nil
	}

	require.NoError(t, w.reconcileSourcePod(context.Background(), pod))

	require.Eventually(t, func() bool {
		return getContent(t, w, content.Name).Status.Source != nil
	}, time.Second, 5*time.Millisecond)
	assertPublishedTheRecordedSource(t, getContent(t, w, content.Name))
}

// The agent can die between committing the artifact and writing Ready, and the
// facts have to survive that: they come from the artifact, not from memory.
func TestArtifactRecoveryPublishesTheSourceItRecorded(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	pod := makeSourcePod()
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "main",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "Error"}},
	}}
	fc := &fakeCheckpointer{}
	w := makeNodeController(t, fc, content, pod)
	committedArtifact(t, w, string(content.UID))

	require.NoError(t, w.reconcileSourcePod(context.Background(), pod))

	assert.False(t, fc.wasCalled())
	assertPublishedTheRecordedSource(t, getContent(t, w, content.Name))
}

// The dump kills the source, so the pod can be gone by the time the work order
// is reconciled again; a committed artifact still publishes.
func TestDeferredCommittedCapturePublishesTheSourceItRecorded(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	fc := &fakeCheckpointer{}
	w := makeNodeController(t, fc, content)
	committedArtifact(t, w, string(content.UID))

	w.reconcilePodSnapshotContent(context.Background(), content.Name)

	assert.False(t, fc.wasCalled())
	got := getContent(t, w, content.Name)
	assert.Nil(t, meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed))
	assertPublishedTheRecordedSource(t, got)
}

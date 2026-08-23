// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

func minimalSnapshotJob() *snapshotv1alpha1.SnapshotJob {
	return &snapshotv1alpha1.SnapshotJob{
		ObjectMeta: metav1.ObjectMeta{Name: "warm-worker", Namespace: "inference", UID: "sj-uid"},
		Spec: snapshotv1alpha1.SnapshotJobSpec{
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "worker", Image: "test:latest"}},
				},
			},
			ActiveDeadlineSeconds: ptr.To(int64(1800)),
			PodSnapshotTemplate: snapshotv1alpha1.PodSnapshotTemplate{
				TargetContainers: []string{"worker"},
			},
		},
	}
}

func TestBuildSourceJob(t *testing.T) {
	t.Run("wires identity, target, and options through to NewCheckpointJob", func(t *testing.T) {
		sj := minimalSnapshotJob()

		job, err := buildSourceJob(sj)
		require.NoError(t, err)

		assert.Equal(t, "warm-worker", job.Name)
		assert.Equal(t, "inference", job.Namespace)
		assert.Equal(t, ptr.To(int64(1800)), job.Spec.ActiveDeadlineSeconds)
		assert.Equal(t, ptr.To(int32(0)), job.Spec.BackoffLimit)
		assert.Nil(t, job.Spec.TTLSecondsAfterFinished, "SnapshotJob cleanup is controller-driven, not TTL-driven")
		assert.Equal(t, "warm-worker", job.Labels[snapshotv1alpha1.CheckpointIDLabel],
			"CheckpointID must be sj.Name, matching the artifact path <basePath>/<sj.Name>/versions/1")

		main := requireContainer(t, job.Spec.Template.Spec.Containers, "worker")
		require.NotNil(t, main.ReadinessProbe, "target container must get the ready-for-snapshot probe")
		require.NotNil(t, job.Spec.Template.Spec.SecurityContext)
		require.NotNil(t, job.Spec.Template.Spec.SecurityContext.SeccompProfile)
		profile := job.Spec.Template.Spec.SecurityContext.SeccompProfile
		assert.Equal(t, corev1.SeccompProfileTypeLocalhost, profile.Type)
		assert.Equal(t, ptr.To(snapshotv1alpha1.DefaultSeccompLocalhostProfile), profile.LocalhostProfile)
	})

	t.Run("stamps the owner label without clobbering existing pod template labels", func(t *testing.T) {
		sj := minimalSnapshotJob()
		sj.Spec.PodTemplate.Labels = map[string]string{"existing": "label"}

		job, err := buildSourceJob(sj)
		require.NoError(t, err)

		assert.Equal(t, "warm-worker", job.Spec.Template.Labels[snapshotv1alpha1.SnapshotJobOwnerLabel])
		assert.Equal(t, "sj-uid", job.Spec.Template.Labels[snapshotv1alpha1.SnapshotJobOwnerUIDLabel])
		assert.Equal(t, "label", job.Spec.Template.Labels["existing"])
	})

	// SnapshotJob must preserve the caller's pod shape. In particular, it may
	// not inject another long-running container that has an independent
	// lifecycle or changes the capture target.
	t.Run("no extra workload containers are injected", func(t *testing.T) {
		sj := minimalSnapshotJob()
		sj.Spec.PodTemplate.Spec.Containers = append(sj.Spec.PodTemplate.Spec.Containers,
			corev1.Container{Name: "helper", Image: "test:latest"})

		job, err := buildSourceJob(sj)
		require.NoError(t, err)

		var names []string
		for _, c := range job.Spec.Template.Spec.Containers {
			names = append(names, c.Name)
		}
		assert.ElementsMatch(t, []string{"worker", "helper"}, names,
			"only the caller's containers may run; an injected extra container would never exit")
		assert.Empty(t, job.Spec.Template.Spec.InitContainers)

		// A restartPolicy other than Never would restart the target after it
		// exits 0, so the pod never reaches Succeeded.
		assert.Equal(t, corev1.RestartPolicyNever, job.Spec.Template.Spec.RestartPolicy)

		// Service-mesh sidecars are the classic cause of a Job that captures
		// fine and then hangs forever: they never exit on their own.
		assert.Equal(t, "disabled", job.Spec.Template.Annotations["linkerd.io/inject"])
		assert.Equal(t, "false", job.Spec.Template.Annotations["sidecar.istio.io/inject"])
	})

	// The workload signals readiness by writing this file, and the agent
	// writes its post-dump sentinel next to it. If the probe and the workload
	// ever disagree on the path, the pod never goes Ready and the capture
	// never starts — a silent hang rather than a failure.
	t.Run("readiness probe reads the ready file from the control mount", func(t *testing.T) {
		sj := minimalSnapshotJob()

		job, err := buildSourceJob(sj)
		require.NoError(t, err)

		main := requireContainer(t, job.Spec.Template.Spec.Containers, "worker")
		require.NotNil(t, main.ReadinessProbe)
		require.NotNil(t, main.ReadinessProbe.Exec)
		assert.Equal(t,
			[]string{"cat", snapshotv1alpha1.SnapshotControlMountPath + "/" + snapshotv1alpha1.ReadyForSnapshotFile},
			main.ReadinessProbe.Exec.Command)

		var mountPaths []string
		for _, m := range main.VolumeMounts {
			mountPaths = append(mountPaths, m.MountPath)
		}
		assert.Contains(t, mountPaths, snapshotv1alpha1.SnapshotControlMountPath,
			"the target container must mount the control volume the probe and sentinel live in")
	})

	t.Run("WrapLaunchJob is always false: command/args pass through unchanged", func(t *testing.T) {
		sj := minimalSnapshotJob()
		sj.Spec.PodTemplate.Spec.Containers[0].Command = []string{"python3", "-m", "worker"}

		job, err := buildSourceJob(sj)
		require.NoError(t, err)

		main := requireContainer(t, job.Spec.Template.Spec.Containers, "worker")
		assert.Equal(t, []string{"python3", "-m", "worker"}, main.Command,
			"PodSnapshotTemplate has no multi-GPU field (spec §5.4) — command must never be wrapped")
	})

	t.Run("SnapshotJob name longer than a label value is a terminal spec error", func(t *testing.T) {
		// The CRD does not constrain metadata.name length (up to 253 chars, RFC
		// 1123 subdomain), but sj.Name becomes a label VALUE (capped at 63, RFC
		// 1123 label value) via SnapshotJobOwnerLabel and CheckpointIDLabel.
		// Without this check, a long-named SnapshotJob would fail Job creation
		// with an apiserver error and retry forever, since that's not an
		// AlreadyExists.
		sj := minimalSnapshotJob()
		sj.Name = strings.Repeat("a", 64)

		_, err := buildSourceJob(sj)
		require.Error(t, err)
	})

	t.Run("valid dotted and 63-character SnapshotJob names are accepted", func(t *testing.T) {
		for _, name := range []string{"warm.worker", strings.Repeat("a", 63)} {
			sj := minimalSnapshotJob()
			sj.Name = name

			_, err := buildSourceJob(sj)
			require.NoError(t, err)
		}
	})

	t.Run("empty targetContainers is a terminal spec error, not a panic", func(t *testing.T) {
		sj := minimalSnapshotJob()
		sj.Spec.PodSnapshotTemplate.TargetContainers = nil

		_, err := buildSourceJob(sj)
		require.Error(t, err)
	})

	t.Run("more than one targetContainers entry is a terminal spec error", func(t *testing.T) {
		// The CRD caps this at MaxItems=1, but this is defense in depth for an
		// object that bypassed CEL validation — v1alpha1 supports exactly one
		// target, so a second entry must not be silently ignored.
		sj := minimalSnapshotJob()
		sj.Spec.PodTemplate.Spec.Containers = append(sj.Spec.PodTemplate.Spec.Containers,
			corev1.Container{Name: "helper", Image: "test:latest"})
		sj.Spec.PodSnapshotTemplate.TargetContainers = []string{"worker", "helper"}

		_, err := buildSourceJob(sj)
		require.Error(t, err)
	})

	t.Run("target container absent from podTemplate is a terminal spec error", func(t *testing.T) {
		sj := minimalSnapshotJob()
		sj.Spec.PodSnapshotTemplate.TargetContainers = []string{"does-not-exist"}

		_, err := buildSourceJob(sj)
		require.Error(t, err)
	})

	t.Run("empty podTemplate containers is a terminal spec error", func(t *testing.T) {
		sj := minimalSnapshotJob()
		sj.Spec.PodTemplate.Spec.Containers = nil

		_, err := buildSourceJob(sj)
		require.Error(t, err)
	})
}

func requireContainer(t *testing.T, containers []corev1.Container, name string) *corev1.Container {
	t.Helper()
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	t.Fatalf("container %q not found in %#v", name, containers)
	return nil
}

// getBatchJobByName finds a Job in a fake client's tracked objects by name. Shared
// across this file and snapshotjob_reconciler_test.go — those tests only care
// whether/what got created, not the full build matrix already covered above.
func getBatchJobByName(jobs *batchv1.JobList, name string) *batchv1.Job {
	for i := range jobs.Items {
		if jobs.Items[i].Name == name {
			return &jobs.Items[i]
		}
	}
	return nil
}

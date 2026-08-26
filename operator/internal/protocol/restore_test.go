// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package protocol

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

func restorePodFixture() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "restore-worker",
			Annotations: map[string]string{"example.com/team": "inference"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:    "main",
					Image:   "worker:latest",
					Command: []string{"python3"},
					Args:    []string{"serve.py"},
				},
				{Name: "sidecar", Image: "sidecar:latest"},
			},
		},
	}
}

func TestNewRestorePodSetsRestoreFromAnnotation(t *testing.T) {
	pod, err := NewRestorePod(restorePodFixture(), PodOptions{
		Namespace:       "inference",
		SnapshotName:    "snapshot-a",
		TargetContainer: "main",
		SeccompProfile:  snapshotv1alpha1.DefaultSeccompLocalhostProfile,
	})
	require.NoError(t, err)

	assert.Equal(t, "inference", pod.Namespace)
	assert.Equal(t, corev1.RestartPolicyNever, pod.Spec.RestartPolicy)
	assert.Equal(t, "snapshot-a", pod.Annotations[snapshotv1alpha1.RestoreFromAnnotation])
	assert.Equal(t, "inference", pod.Annotations["example.com/team"])

	main := &pod.Spec.Containers[0]
	assert.Equal(t, []string{"python3"}, main.Command)
	assert.Equal(t, []string{"serve.py"}, main.Args)
	assert.Equal(t, "1", envValue(main.Env, RestoreStandbyModeEnv))
	assert.Equal(t, snapshotv1alpha1.SnapshotControlMountPath, main.VolumeMounts[0].MountPath)
	assert.Equal(t, "main", main.VolumeMounts[0].SubPath)
	require.NotNil(t, main.StartupProbe)

	sidecar := &pod.Spec.Containers[1]
	assert.Empty(t, sidecar.VolumeMounts)
	assert.Empty(t, sidecar.Env)
	assert.Nil(t, sidecar.StartupProbe)
	require.NoError(t, ValidateRestorePodSpec(&pod.Spec, "main", snapshotv1alpha1.DefaultSeccompLocalhostProfile))
}

func TestPrepareRestorePodSpecRequiresCapturedContainer(t *testing.T) {
	spec := restorePodFixture().Spec
	require.Error(t, PrepareRestorePodSpec(&spec, "", "", true))
	err := PrepareRestorePodSpec(&spec, "missing", "", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `container "missing"`)
}

func TestPrepareRestorePodSpecIsIdempotent(t *testing.T) {
	spec := restorePodFixture().Spec
	require.NoError(t, PrepareRestorePodSpec(&spec, "main", "", true))
	require.NoError(t, PrepareRestorePodSpec(&spec, "main", "", true))

	main := &spec.Containers[0]
	assert.Len(t, spec.Volumes, 1)
	assert.Len(t, main.VolumeMounts, 1)
	assert.Equal(t, "1", envValue(main.Env, RestoreStandbyModeEnv))
}

func TestPrepareRestorePodSpecReusesExistingProbe(t *testing.T) {
	spec := restorePodFixture().Spec
	spec.Containers[0].ReadinessProbe = &corev1.Probe{
		ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/ready"}},
		InitialDelaySeconds: 30,
		PeriodSeconds:       10,
		FailureThreshold:    3,
	}
	require.NoError(t, PrepareRestorePodSpec(&spec, "main", "", true))
	startup := spec.Containers[0].StartupProbe
	require.NotNil(t, startup)
	require.NotNil(t, startup.HTTPGet)
	assert.Equal(t, "/ready", startup.HTTPGet.Path)
	assert.Zero(t, startup.InitialDelaySeconds)
	assert.Equal(t, int32(1), startup.PeriodSeconds)
	assert.Equal(t, int32(restoreStartupFailureThreshold), startup.FailureThreshold)
}

func TestValidateRestorePodSpecFailures(t *testing.T) {
	spec := restorePodFixture().Spec
	require.NoError(t, PrepareRestorePodSpec(&spec, "main", snapshotv1alpha1.DefaultSeccompLocalhostProfile, true))

	tests := map[string]func(*corev1.PodSpec){
		"volume": func(s *corev1.PodSpec) { s.Volumes = nil },
		"mount":  func(s *corev1.PodSpec) { s.Containers[0].VolumeMounts = nil },
		"env": func(s *corev1.PodSpec) {
			s.Containers[0].Env = nil
		},
		"probe": func(s *corev1.PodSpec) { s.Containers[0].StartupProbe = nil },
		"seccomp": func(s *corev1.PodSpec) {
			s.SecurityContext = nil
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			bad := spec.DeepCopy()
			mutate(bad)
			require.Error(t, ValidateRestorePodSpec(bad, "main", snapshotv1alpha1.DefaultSeccompLocalhostProfile))
		})
	}
}

func envValue(env []corev1.EnvVar, name string) string {
	for _, item := range env {
		if item.Name == name {
			return item.Value
		}
	}
	return ""
}

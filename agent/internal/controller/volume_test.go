// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

func TestDeclaredVolumesForContainer(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					VolumeMounts: []corev1.VolumeMount{
						{Name: "model", MountPath: "/model-cache", ReadOnly: true, SubPath: "weights"},
						{Name: "scratch", MountPath: "/scratch"},
						{Name: "config", MountPath: "/etc/dynamo", SubPathExpr: "$(POD_NAME)"},
						{Name: "unknown", MountPath: "/unknown"},
						{Name: "snapshot-control", MountPath: "/snapshot-control"},
						{Name: "checkpoint-storage", MountPath: "/checkpoints"},
						{Name: "kube-api-access-abc", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount"},
						{Name: "custom-token", MountPath: "/var/run/secrets/custom"},
					},
				},
				{Name: "sidecar", VolumeMounts: []corev1.VolumeMount{{Name: "model", MountPath: "/other"}}},
			},
			Volumes: []corev1.Volume{
				{Name: "model", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "model-pvc"}}},
				{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "dynamo-config"}}}},
				{Name: "snapshot-control", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "checkpoint-storage", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "snapshot-pvc"}}},
				{Name: "kube-api-access-abc", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{
						{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"}},
						{ConfigMap: &corev1.ConfigMapProjection{
							LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"},
							Items:                []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
						}},
						{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{
							Path:     "namespace",
							FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
						}}}},
					},
				}}},
				{Name: "custom-token", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
						Path:     "token",
						Audience: "custom-audience",
					}}},
				}}},
			},
		},
	}

	volumes := declaredVolumesForContainer(pod, "main")

	require.Len(t, volumes, 8)
	assert.Equal(t, []snapshotv1alpha1.CheckpointSourceDeclaredVolume{
		{
			Path:         "/model-cache",
			Volume:       "model",
			VolumeSource: "PersistentVolumeClaim/model-pvc",
		},
		{
			Path:         "/scratch",
			Volume:       "scratch",
			VolumeSource: "EmptyDir",
		},
		{
			Path:         "/etc/dynamo",
			Volume:       "config",
			VolumeSource: "ConfigMap/dynamo-config",
		},
		{
			Path:         "/unknown",
			Volume:       "unknown",
			VolumeSource: "Volume/unknown",
		},
		{
			Path:         "/snapshot-control",
			Volume:       "snapshot-control",
			VolumeSource: "EmptyDir",
		},
		{
			Path:         "/checkpoints",
			Volume:       "checkpoint-storage",
			VolumeSource: "PersistentVolumeClaim/snapshot-pvc",
		},
		{
			Path:         "/var/run/secrets/kubernetes.io/serviceaccount",
			Volume:       "kube-api-access-abc",
			VolumeSource: "Projected",
		},
		{
			Path:         "/var/run/secrets/custom",
			Volume:       "custom-token",
			VolumeSource: "Projected",
		},
	}, volumes)

	assert.Equal(t, []snapshotv1alpha1.CheckpointSourceDeclaredVolume{{
		Path:         "/other",
		Volume:       "model",
		VolumeSource: "PersistentVolumeClaim/model-pvc",
	}}, declaredVolumesForContainer(pod, "sidecar"))
}

func TestDeclaredVolumesForContainerMissingContainer(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main"}},
		},
	}

	assert.Nil(t, declaredVolumesForContainer(pod, "missing"))
}

func TestDeclaredVolumesForContainerRecordsKnownZero(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main"}},
		},
	}

	volumes := declaredVolumesForContainer(pod, "main")

	require.NotNil(t, volumes)
	assert.Empty(t, volumes)
}

func TestDescribeVolumeSource(t *testing.T) {
	tests := []struct {
		name   string
		volume corev1.Volume
		want   string
	}{
		{
			name: "persistent volume claim",
			volume: corev1.Volume{VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "model-pvc"},
			}},
			want: "PersistentVolumeClaim/model-pvc",
		},
		{
			name: "config map",
			volume: corev1.Volume{VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "dynamo-config"},
				},
			}},
			want: "ConfigMap/dynamo-config",
		},
		{
			name: "secret",
			volume: corev1.Volume{VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: "credentials"},
			}},
			want: "Secret/credentials",
		},
		{
			name: "host path",
			volume: corev1.Volume{VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/models"},
			}},
			want: "HostPath//models",
		},
		{
			name: "CSI",
			volume: corev1.Volume{VolumeSource: corev1.VolumeSource{
				CSI: &corev1.CSIVolumeSource{Driver: "csi.example.com"},
			}},
			want: "CSI/csi.example.com",
		},
		{
			name: "NFS",
			volume: corev1.Volume{VolumeSource: corev1.VolumeSource{
				NFS: &corev1.NFSVolumeSource{Server: "nfs.example.com"},
			}},
			want: "NFS/nfs.example.com",
		},
		{
			name:   "empty directory",
			volume: corev1.Volume{VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			want:   "EmptyDir",
		},
		{
			name:   "projected",
			volume: corev1.Volume{VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{}}},
			want:   "Projected",
		},
		{
			name:   "downward API",
			volume: corev1.Volume{VolumeSource: corev1.VolumeSource{DownwardAPI: &corev1.DownwardAPIVolumeSource{}}},
			want:   "DownwardAPI",
		},
		{
			name:   "ephemeral",
			volume: corev1.Volume{VolumeSource: corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{}}},
			want:   "Ephemeral",
		},
		{
			name:   "unknown",
			volume: corev1.Volume{Name: "custom"},
			want:   "Volume/custom",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, describeVolumeSource(test.volume))
		})
	}
}

func TestNamedVolumeSource(t *testing.T) {
	assert.Equal(t, "Secret/credentials", namedVolumeSource("Secret", "credentials"))
	assert.Equal(t, "EmptyDir", namedVolumeSource("EmptyDir", ""))
}

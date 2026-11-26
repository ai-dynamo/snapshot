// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

func manifestPod(seccomp *corev1.SeccompProfile) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "worker"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "busybox"}},
		},
	}
	if seccomp != nil {
		pod.Spec.SecurityContext = &corev1.PodSecurityContext{SeccompProfile: seccomp}
		pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{SeccompProfile: seccomp}
	}
	return pod
}

var (
	runtimeDefaultProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	blockIOUringProfile   = &corev1.SeccompProfile{
		Type:             corev1.SeccompProfileTypeLocalhost,
		LocalhostProfile: ptr.To(podcontract.DefaultSeccompLocalhostProfile),
	}
)

func TestBuildCheckpointJobSeccompProfile(t *testing.T) {
	tests := []struct {
		name     string
		apply    bool
		manifest *corev1.SeccompProfile
		want     *corev1.SeccompProfile
	}{
		{name: "default sets the block-iouring profile", apply: true, want: blockIOUringProfile},
		{name: "off sets none", apply: false},
		{name: "off keeps the manifest profile", apply: false, manifest: runtimeDefaultProfile, want: runtimeDefaultProfile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job, err := buildCheckpointJob(manifestPod(tt.manifest), "default", "main", "worker-capture-abcde",
				checkpointOptions{SeccompProfile: seccompProfile(tt.apply)})
			require.NoError(t, err)

			var got *corev1.SeccompProfile
			if sc := job.Spec.Template.Spec.SecurityContext; sc != nil {
				got = sc.SeccompProfile
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestBuildRestorePodSeccompProfile(t *testing.T) {
	tests := []struct {
		name     string
		apply    bool
		manifest *corev1.SeccompProfile
		want     *corev1.SeccompProfile
	}{
		{name: "default sets the block-iouring profile", apply: true, want: blockIOUringProfile},
		{name: "off sets none", apply: false},
		{name: "off keeps the manifest profile", apply: false, manifest: runtimeDefaultProfile, want: runtimeDefaultProfile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod, err := buildRestorePod(manifestPod(tt.manifest), "default", "worker-snapshot", "main", seccompProfile(tt.apply))
			require.NoError(t, err)

			require.Len(t, pod.Spec.Containers, 1)
			var got *corev1.SeccompProfile
			if sc := pod.Spec.Containers[0].SecurityContext; sc != nil {
				got = sc.SeccompProfile
			}
			assert.Equal(t, tt.want, got)
			assert.Equal(t, "worker-snapshot", pod.Annotations[podcontract.RestoreFromAnnotation])
		})
	}
}

func TestBuildRestorePodRefusesManifestProfileWhenApplied(t *testing.T) {
	_, err := buildRestorePod(manifestPod(runtimeDefaultProfile), "default", "worker-snapshot", "main", seccompProfile(true))
	require.ErrorContains(t, err, podcontract.DefaultSeccompLocalhostProfile)
}

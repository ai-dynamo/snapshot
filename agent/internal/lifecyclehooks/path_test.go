// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecyclehooks

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

func TestCheckWritePath(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: "main",
		VolumeMounts: []corev1.VolumeMount{
			{Name: podcontract.SnapshotControlVolumeName, MountPath: podcontract.SnapshotControlMountPath, SubPath: "main"},
			{Name: "data", MountPath: "/data"},
		},
	}}}}
	for _, tc := range []struct {
		name string
		path string
		ok   bool
	}{
		{"container filesystem", "/tmp/released", true},
		{"any volume", "/data/released", true},
		{"own file in the control volume", podcontract.SnapshotControlMountPath + "/released", true},
		{"Snapshot's file in the control volume", podcontract.SnapshotControlMountPath + "/" + podcontract.RestoreCompleteFile, false},
		{"Snapshot's file name elsewhere", "/data/" + podcontract.RestoreCompleteFile, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := CheckWritePath(pod, "main", tc.path); (err == nil) != tc.ok {
				t.Fatalf("CheckWritePath(%s) = %v, want ok=%v", tc.path, err, tc.ok)
			}
		})
	}
	if err := CheckWritePath(pod, "other", "/tmp/released"); err == nil {
		t.Fatal("CheckWritePath accepted a container that is not in the Pod")
	}
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecyclehooks

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

func hooksPod() *corev1.Pod {
	return &corev1.Pod{Spec: corev1.PodSpec{
		Containers: []corev1.Container{{
			Name: "main",
			VolumeMounts: []corev1.VolumeMount{
				{Name: podcontract.SnapshotControlVolumeName, MountPath: podcontract.SnapshotControlMountPath, SubPath: "main"},
				{Name: "scratch", MountPath: "/scratch"},
				{Name: "data", MountPath: "/data"},
				{Name: "host", MountPath: "/host-logs"},
				{Name: "config", MountPath: "/etc/app"},
				// An emptyDir mounted inside a PVC mount: the nested one wins.
				{Name: "scratch", MountPath: "/data/tmp/"},
			},
		}},
		Volumes: []corev1.Volume{
			{Name: podcontract.SnapshotControlVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}},
			{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}}},
			{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/log"}}},
			{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}},
		},
	}}
}

func TestCheckPath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		path  string
		write bool
		ok    bool
	}{
		{"container filesystem", "/tmp/ready", true, true},
		{"emptyDir", "/scratch/released", true, true},
		{"emptyDir mount point itself", "/scratch", false, true},
		{"emptyDir nested in a PVC mount", "/data/tmp/released", true, true},
		{"name that starts like a mount", "/database/ready", true, true},
		{"PVC", "/data/released", true, false},
		{"PVC, read", "/data/ready", false, false},
		{"hostPath", "/host-logs/released", true, false},
		{"hostPath, nested", "/host-logs/a/b/ready", false, false},
		{"other volume type", "/etc/app/ready", false, false},
		{"own file in the control volume", "/snapshot-control/released", true, true},
		{"reserved file, write", "/snapshot-control/" + podcontract.RestoreCompleteFile, true, false},
		{"reserved file, read", "/snapshot-control/" + podcontract.RestoreCompleteFile, false, true},
		{"ready-for-snapshot, read", "/snapshot-control/" + podcontract.ReadyForSnapshotFile, false, true},
		{"reserved name outside the control volume", "/tmp/" + podcontract.RestoreCompleteFile, true, true},
		{"relative", "tmp/ready", false, false},
		{"not clean", "/tmp/../data/ready", false, false},
		{"trailing slash", "/tmp/ready/", false, false},
		{"root", "/", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckPath(hooksPod(), "main", tc.path, tc.write)
			if tc.ok && err != nil {
				t.Fatalf("CheckPath(%q, write=%v) = %v, want nil", tc.path, tc.write, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("CheckPath(%q, write=%v) = nil, want an error", tc.path, tc.write)
			}
		})
	}
}

func TestCheckPathNeedsTheContainerAndItsVolume(t *testing.T) {
	if err := CheckPath(hooksPod(), "other", "/tmp/ready", false); err == nil {
		t.Error("CheckPath accepted a container that is not in the Pod")
	}
	pod := hooksPod()
	pod.Spec.Volumes = pod.Spec.Volumes[1:] // drop the control volume
	if err := CheckPath(pod, "main", "/snapshot-control/released", true); err == nil {
		t.Error("CheckPath accepted a mount whose volume is not in the Pod spec")
	}
}

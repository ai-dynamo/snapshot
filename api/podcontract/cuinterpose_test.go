// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestParseCuInterposeAnnotation(t *testing.T) {
	if enabled, err := ParseCuInterposeAnnotation(nil); enabled || err != nil {
		t.Fatalf("absent annotation = %v, %v", enabled, err)
	}
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"enabled", true}, {" enabled ", true},
		{"disabled", false}, {" disabled ", false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			annotations := map[string]string{CuInterposeAnnotation: tc.value}
			got, err := ParseCuInterposeAnnotation(annotations)
			if err != nil || got != tc.want {
				t.Fatalf("ParseCuInterposeAnnotation(%q) = %v, %v, want %v", tc.value, got, err, tc.want)
			}
		})
	}
	for _, value := range []string{"", " ", "true", "false", "1", "0", "ENABLED", "DISABLED", "yes"} {
		annotations := map[string]string{CuInterposeAnnotation: value}
		_, err := ParseCuInterposeAnnotation(annotations)
		if err == nil || !strings.Contains(err.Error(), CuInterposeAnnotation) {
			t.Fatalf("invalid annotation %q should identify its key: %v", value, err)
		}
	}
}

func TestHasCuInterposeLauncher(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want bool
	}{
		{name: "launched workload", argv: append(CuInterposeLauncherPrefix(), "worker"), want: true},
		{name: "no workload", argv: CuInterposeLauncherPrefix()},
		{name: "no launcher", argv: []string{"worker"}},
		{name: "another library", argv: []string{CuInterposeLauncherPath, "--library", "/lib/other.so", "--", "worker"}},
		{name: "empty", argv: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasCuInterposeLauncher(tc.argv); got != tc.want {
				t.Fatalf("HasCuInterposeLauncher(%q) = %v, want %v", tc.argv, got, tc.want)
			}
		})
	}
}

func TestIsCuInterposeDeliveryPath(t *testing.T) {
	for mountPath, want := range map[string]bool{
		CuInterposeMountPath:          true,
		CuInterposeMountPath + "/":    true,
		CuInterposeMountPath + "/lib": true,
		"/tmp/snapshot-cuda-other":    false,
		"/tmp":                        false,
		SnapshotControlMountPath:      false,
	} {
		if got := IsCuInterposeDeliveryPath(mountPath); got != want {
			t.Errorf("IsCuInterposeDeliveryPath(%q) = %v, want %v", mountPath, got, want)
		}
	}
}

func TestValidateCuInterposeTarget(t *testing.T) {
	control := corev1.VolumeMount{Name: SnapshotControlVolumeName, MountPath: SnapshotControlMountPath, SubPath: "main"}
	delivery := corev1.VolumeMount{Name: "snapshot-cuda", MountPath: CuInterposeMountPath, ReadOnly: true}
	launched := append(CuInterposeLauncherPrefix(), "worker")
	for _, tc := range []struct {
		name          string
		command, args []string
		mounts        []corev1.VolumeMount
		wantError     string
	}{
		{name: "agent delivers", command: []string{"worker"}, mounts: []corev1.VolumeMount{control}},
		{name: "agent delivers to image entrypoint", mounts: []corev1.VolumeMount{control}},
		{name: "self-delivered in command", command: launched, mounts: []corev1.VolumeMount{delivery}},
		{
			name: "self-delivered in args", command: CuInterposeLauncherPrefix(), args: []string{"worker"},
			mounts: []corev1.VolumeMount{delivery},
		},
		{
			name: "launcher only in args", args: launched,
			mounts: []corev1.VolumeMount{control, delivery}, wantError: "must start with",
		},
		{name: "no control mount", command: []string{"worker"}, wantError: "does not mount"},
		{
			name: "delivery mount without launcher", command: []string{"worker"},
			mounts: []corev1.VolumeMount{control, delivery}, wantError: "must start with",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			container := &corev1.Container{Name: "main", Command: tc.command, Args: tc.args, VolumeMounts: tc.mounts}
			err := ValidateCuInterposeTarget(container)
			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("ValidateCuInterposeTarget() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("ValidateCuInterposeTarget() = %v, want error containing %q", err, tc.wantError)
			}
		})
	}
}

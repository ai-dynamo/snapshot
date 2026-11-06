// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecyclehooks

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

func TestReadyCondition(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase corev1.PodPhase
		ready bool
		want  bool
	}{
		{"ready in a running Pod", corev1.PodRunning, true, true},
		{"not ready", corev1.PodRunning, false, false},
		{"ready in a Pod that isn't running", corev1.PodPending, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{Status: corev1.PodStatus{Phase: tc.phase}}
			status := &corev1.ContainerStatus{Name: "main", Ready: tc.ready}
			if err := (ReadyCondition{}).Probe(t.Context(), pod, status, 0); (err == nil) != tc.want {
				t.Fatalf("Probe = %v, want ready %v", err, tc.want)
			}
		})
	}
}

func TestFileProbe(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	if err := os.WriteFile(ready, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	status := &corev1.ContainerStatus{Name: "main", ContainerID: "containerd://abc"}
	for path, want := range map[string]bool{ready: true, filepath.Join(dir, "missing"): false} {
		probe := &FileProbe{Exec: &localShell{}, Path: path}
		if err := probe.Probe(t.Context(), &corev1.Pod{}, status, 5*time.Second); (err == nil) != want {
			t.Fatalf("Probe(%s) = %v, want success %v", path, err, want)
		}
	}
}

func TestHTTPProbeCheck(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: "main", Ports: []corev1.ContainerPort{{Name: "hooks", ContainerPort: 8000}},
	}}}}
	for _, tc := range []struct {
		name   string
		action snapshotv1alpha1.HTTPGetAction
		ok     bool
	}{
		{"named port", snapshotv1alpha1.HTTPGetAction{Path: "/status", Port: intstr.FromString("hooks")}, true},
		{"unknown port name", snapshotv1alpha1.HTTPGetAction{Path: "/status", Port: intstr.FromString("metrics")}, false},
		{"path that doesn't parse", snapshotv1alpha1.HTTPGetAction{Path: "/%zz", Port: intstr.FromInt32(8000)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := (&HTTPProbe{Action: &tc.action}).Check(pod, "main"); (err == nil) != tc.ok {
				t.Fatalf("Check = %v, want ok %v", err, tc.ok)
			}
		})
	}
}

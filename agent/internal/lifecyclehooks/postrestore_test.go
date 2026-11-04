// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecyclehooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

func TestFilePostRestore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "released")
	h := &FilePostRestore{Exec: &localShell{}, Path: path, Timeout: 5 * time.Second}
	target := PostRestoreTarget{Container: "main", ContainerID: "ctr"}

	if err := h.Notify(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the file wasn't created: %v", err)
	}
	// An existing file counts as the notification, so a repeat succeeds.
	if err := h.Notify(t.Context(), target); err != nil {
		t.Fatalf("Notify = %v, want nil for an existing file", err)
	}

	// A file that can't be created fails with the reason.
	missing := &FilePostRestore{Exec: &localShell{}, Path: filepath.Join(dir, "no", "released"), Timeout: 5 * time.Second}
	if err := missing.Notify(t.Context(), target); err == nil || !strings.Contains(err.Error(), "failed to create") {
		t.Fatalf("Notify = %v, want a failure to create the file", err)
	}
}

func TestHTTPPostRestoreCheck(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: "main", Ports: []corev1.ContainerPort{{Name: "hooks", ContainerPort: 8000}},
	}}}}
	for _, tc := range []struct {
		name   string
		action snapshotv1alpha1.HTTPPostAction
		ok     bool
	}{
		{"named port", snapshotv1alpha1.HTTPPostAction{Path: "/resume", Port: intstr.FromString("hooks")}, true},
		{"unknown port name", snapshotv1alpha1.HTTPPostAction{Path: "/resume", Port: intstr.FromString("metrics")}, false},
		{"path that doesn't parse", snapshotv1alpha1.HTTPPostAction{Path: "/%zz", Port: intstr.FromInt32(8000)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := (&HTTPPostRestore{Action: &tc.action}).Check(pod, "main"); (err == nil) != tc.ok {
				t.Fatalf("Check = %v, want ok %v", err, tc.ok)
			}
		})
	}
}

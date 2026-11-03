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

	if err := h.Recheck(t.Context(), target); err != nil {
		t.Fatalf("Recheck = %v, want nil before the file exists", err)
	}
	if err := h.Notify(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the file wasn't created: %v", err)
	}
	// An earlier attempt created it, so a repeat succeeds.
	if err := h.Notify(t.Context(), target); err != nil {
		t.Fatalf("Notify = %v, want nil for the file an earlier attempt created", err)
	}
	// Before the release, the same file means it was there before Snapshot.
	if err := h.Recheck(t.Context(), target); err == nil || !strings.Contains(err.Error(), "must not exist before Snapshot creates it") {
		t.Fatalf("Recheck = %v, want an error for an existing file", err)
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

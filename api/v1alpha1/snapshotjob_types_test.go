// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TestSchemeRegistersSnapshotJobKinds verifies AddToScheme exposes SnapshotJob
// and SnapshotJobList alongside the existing snapshot kinds.
func TestSchemeRegistersSnapshotJobKinds(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme failed: %v", err)
	}
	for _, kind := range []string{"SnapshotJob", "SnapshotJobList"} {
		if !scheme.Recognizes(GroupVersion.WithKind(kind)) {
			t.Errorf("scheme does not recognize kind %q in %s", kind, GroupVersion.String())
		}
	}
}

// TestSnapshotJobDeepCopyIsIndependent verifies the generated deepcopy produces
// an equal but independent SnapshotJob (mutating the clone must not touch the
// source), including through the embedded corev1.PodTemplateSpec.
func TestSnapshotJobDeepCopyIsIndependent(t *testing.T) {
	original := &SnapshotJob{
		ObjectMeta: metav1.ObjectMeta{Name: "warm-worker", Namespace: "inference"},
		Spec: SnapshotJobSpec{
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "worker"}},
				},
			},
			PodSnapshotTemplate: PodSnapshotTemplate{
				Metadata: &PodSnapshotTemplateMetadata{
					Labels:      map[string]string{"dynamo.nvidia.com/worker-generation": "abc123"},
					Annotations: map[string]string{"dynamo.nvidia.com/gms-mode": "enabled"},
				},
				TargetContainers: []string{"worker"},
			},
		},
		Status: SnapshotJobStatus{
			PodSnapshotName: "warm-worker-snapshot",
			PodSnapshotUID:  "pod-snapshot-uid",
			SourceJobUID:    "source-job-uid",
			Conditions:      []metav1.Condition{{Type: SnapshotJobConditionRunning, Status: metav1.ConditionTrue, Reason: ReasonPodReady}},
		},
	}

	clone := original.DeepCopy()
	if !reflect.DeepEqual(original, clone) {
		t.Fatalf("DeepCopy is not equal to original")
	}

	clone.Spec.PodTemplate.Spec.Containers[0].Name = "mutated"
	clone.Spec.PodSnapshotTemplate.TargetContainers[0] = "mutated"
	clone.Spec.PodSnapshotTemplate.Metadata.Labels["dynamo.nvidia.com/worker-generation"] = "changed"
	clone.Spec.PodSnapshotTemplate.Metadata.Annotations["dynamo.nvidia.com/gms-mode"] = "disabled"
	clone.Status.Conditions[0].Reason = "Changed"
	if original.Spec.PodTemplate.Spec.Containers[0].Name != "worker" {
		t.Errorf("mutating clone PodTemplate changed original: got %q", original.Spec.PodTemplate.Spec.Containers[0].Name)
	}
	if original.Spec.PodSnapshotTemplate.TargetContainers[0] != "worker" {
		t.Errorf("mutating clone TargetContainers changed original: got %q", original.Spec.PodSnapshotTemplate.TargetContainers[0])
	}
	if original.Spec.PodSnapshotTemplate.Metadata.Labels["dynamo.nvidia.com/worker-generation"] != "abc123" {
		t.Errorf("mutating clone labels changed original: got %q", original.Spec.PodSnapshotTemplate.Metadata.Labels["dynamo.nvidia.com/worker-generation"])
	}
	if original.Spec.PodSnapshotTemplate.Metadata.Annotations["dynamo.nvidia.com/gms-mode"] != "enabled" {
		t.Errorf("mutating clone annotations changed original: got %q", original.Spec.PodSnapshotTemplate.Metadata.Annotations["dynamo.nvidia.com/gms-mode"])
	}
	if original.Status.Conditions[0].Reason != ReasonPodReady {
		t.Errorf("mutating clone condition changed original: got %q", original.Status.Conditions[0].Reason)
	}
}

func setCondition(j *SnapshotJob, conditionType string, status metav1.ConditionStatus, reason string) {
	meta.SetStatusCondition(&j.Status.Conditions, metav1.Condition{
		Type:    conditionType,
		Status:  status,
		Reason:  reason,
		Message: reason,
	})
}

func TestIsSnapshotJobCompleted(t *testing.T) {
	j := &SnapshotJob{}
	if IsSnapshotJobCompleted(j) {
		t.Fatal("expected false for a SnapshotJob with no conditions")
	}
	setCondition(j, SnapshotJobConditionCompleted, metav1.ConditionTrue, ReasonJobCompleted)
	if !IsSnapshotJobCompleted(j) {
		t.Fatal("expected true after Completed=True")
	}
}

func TestIsSnapshotJobFailed(t *testing.T) {
	j := &SnapshotJob{}
	if IsSnapshotJobFailed(j) {
		t.Fatal("expected false for a SnapshotJob with no conditions")
	}
	setCondition(j, SnapshotJobConditionFailed, metav1.ConditionTrue, ReasonJobFailed)
	if !IsSnapshotJobFailed(j) {
		t.Fatal("expected true after Failed=True")
	}
}

func TestIsSnapshotJobTerminal(t *testing.T) {
	cases := []struct {
		name      string
		condition string
		want      bool
	}{
		{"neither set", "", false},
		{"completed", SnapshotJobConditionCompleted, true},
		{"failed", SnapshotJobConditionFailed, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := &SnapshotJob{}
			if tc.condition != "" {
				setCondition(j, tc.condition, metav1.ConditionTrue, "Reason")
			}
			if got := IsSnapshotJobTerminal(j); got != tc.want {
				t.Errorf("IsSnapshotJobTerminal() = %v, want %v", got, tc.want)
			}
		})
	}
}

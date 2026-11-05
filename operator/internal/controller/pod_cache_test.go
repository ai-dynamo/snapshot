// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

func TestTrimPodForCacheKeepsWhatControllersRead(t *testing.T) {
	now := metav1.Now()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "src",
			Namespace:         "ns",
			UID:               "uid",
			Labels:            map[string]string{"l": "v"},
			Annotations:       map[string]string{"a": "true"},
			OwnerReferences:   []metav1.OwnerReference{{Name: "job"}},
			DeletionTimestamp: &now,
			ManagedFields:     []metav1.ManagedFieldsEntry{{Manager: "kubelet"}},
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{{Name: "main", Image: "img", Env: []corev1.EnvVar{{Name: "K", Value: "V"}}}},
			Volumes:    []corev1.Volume{{Name: "v"}},
		},
		Status: corev1.PodStatus{
			Phase:  corev1.PodRunning,
			PodIP:  "10.0.0.7",
			PodIPs: []corev1.PodIP{{IP: "10.0.0.7"}},
			HostIP: "192.168.0.1",
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
				{Type: corev1.PodReady, Status: corev1.ConditionTrue, Reason: "r"},
				{Type: corev1.ContainersReady, Status: corev1.ConditionTrue},
				{Type: podcontract.RestoreStartedCondition, Status: corev1.ConditionTrue},
				{Type: podcontract.RestoredCondition, Status: corev1.ConditionFalse, Reason: podcontract.RestoreReasonFailed},
				{Type: podcontract.GroupRestoreFailedCondition, Status: corev1.ConditionTrue, Reason: "PodIPChanged"},
				{Type: "example.com/Other", Status: corev1.ConditionTrue},
			},
			ContainerStatuses:     []corev1.ContainerStatus{{Name: "main", Ready: true}},
			InitContainerStatuses: []corev1.ContainerStatus{{Name: "init"}},
		},
	}

	out, err := TrimPodForCache(pod)
	if err != nil {
		t.Fatal(err)
	}
	got := out.(*corev1.Pod)
	if got.Name != "src" || got.Namespace != "ns" || got.UID != "uid" ||
		got.Labels["l"] != "v" || got.Annotations["a"] != "true" ||
		len(got.OwnerReferences) != 1 || got.DeletionTimestamp == nil {
		t.Fatalf("metadata lost: %+v", got.ObjectMeta)
	}
	if got.ManagedFields != nil {
		t.Fatalf("managedFields kept")
	}
	if got.Spec.NodeName != "node-1" || got.Spec.Containers != nil || got.Spec.Volumes != nil {
		t.Fatalf("spec = %+v, want only nodeName", got.Spec)
	}
	if got.Status.Phase != corev1.PodRunning || got.Status.PodIP != "10.0.0.7" || !containerReady(got, "main") ||
		len(got.Status.InitContainerStatuses) != 1 || got.Status.PodIPs != nil || got.Status.HostIP != "" {
		t.Fatalf("status = %+v", got.Status)
	}
	// Only the conditions the pod-group controllers read survive: Ready, for
	// the agent gate of admission, and the restore conditions a
	// PodGroupRestore aggregates.
	var kept []corev1.PodConditionType
	for _, c := range got.Status.Conditions {
		kept = append(kept, c.Type)
	}
	want := []corev1.PodConditionType{corev1.PodReady, podcontract.RestoreStartedCondition,
		podcontract.RestoredCondition, podcontract.GroupRestoreFailedCondition}
	if !slices.Equal(kept, want) || !isPodReady(got) || got.Status.Conditions[2].Reason != podcontract.RestoreReasonFailed {
		t.Fatalf("conditions = %+v, want %v", got.Status.Conditions, want)
	}
}

func TestIsPodReady(t *testing.T) {
	for _, tc := range []struct {
		name       string
		conditions []corev1.PodCondition
		want       bool
	}{
		{"no conditions", nil, false},
		{"ready", []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, true},
		{"not ready", []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}, false},
		{"only other conditions", []corev1.PodCondition{{Type: corev1.ContainersReady, Status: corev1.ConditionTrue}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{Status: corev1.PodStatus{Conditions: tc.conditions}}
			if got := isPodReady(pod); got != tc.want {
				t.Fatalf("isPodReady = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTrimPodForCacheLeavesOtherObjects(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n"}}
	out, err := TrimPodForCache(node)
	if err != nil || out != node {
		t.Fatalf("got %v, %v", out, err)
	}
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
			Phase:                 corev1.PodRunning,
			Conditions:            []corev1.PodCondition{{Type: corev1.PodReady}},
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
	if got.Status.Phase != corev1.PodRunning || !containerReady(got, "main") ||
		len(got.Status.InitContainerStatuses) != 1 || got.Status.Conditions != nil {
		t.Fatalf("status = %+v", got.Status)
	}
}

func TestTrimPodForCacheLeavesOtherObjects(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n"}}
	out, err := TrimPodForCache(node)
	if err != nil || out != node {
		t.Fatalf("got %v, %v", out, err)
	}
}

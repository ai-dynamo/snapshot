// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TestSchemeRegistersPodGroupRestoreKinds verifies AddToScheme exposes
// PodGroupRestore and PodGroupRestoreList.
func TestSchemeRegistersPodGroupRestoreKinds(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme failed: %v", err)
	}
	for _, kind := range []string{KindPodGroupRestore, KindPodGroupRestore + "List"} {
		if !scheme.Recognizes(GroupVersion.WithKind(kind)) {
			t.Errorf("scheme does not recognize kind %q in %s", kind, GroupVersion.String())
		}
	}
}

// TestPodGroupRestoreDeepCopyIsIndependent verifies the generated deepcopy
// produces an equal but independent PodGroupRestore, including through the
// targets' container mappings and the member statuses' pointers and lists.
func TestPodGroupRestoreDeepCopyIsIndependent(t *testing.T) {
	startedAt := metav1.NewTime(time.Date(2026, time.October, 16, 18, 20, 40, 0, time.UTC))
	original := &PodGroupRestore{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-tp8-r1", Namespace: "serving"},
		Spec: PodGroupRestoreSpec{
			SnapshotRef:     ObjectIdentity{Name: "llm-tp8", UID: "snapshot-uid"},
			DeadlineSeconds: 1500,
			Targets: []PodGroupRestoreTarget{{
				ID:                "worker-0",
				PodRef:            TargetPodReference{Name: "llm-worker-0-r1"},
				ContainerMappings: []ContainerMapping{{Source: "main", Destination: "server"}},
			}},
		},
		Status: PodGroupRestoreStatus{
			Members: []PodGroupRestoreMemberStatus{{
				ID:          "worker-0",
				Pod:         &ObjectIdentity{Name: "llm-worker-0-r1", UID: "pod-uid"},
				NodeName:    "node-c",
				Containers:  []PodGroupRestoreContainerStatus{{Name: "server", ContainerID: "containerd://c1"}},
				TargetPodIP: "10.1.9.2",
				StartedAt:   &startedAt,
				Phase:       RestorePhaseReleased,
			}},
			Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Released"}},
		},
	}

	clone := original.DeepCopy()
	if !reflect.DeepEqual(original, clone) {
		t.Fatalf("DeepCopy is not equal to original")
	}

	clone.Spec.Targets[0].PodRef.Name = "mutated"
	clone.Spec.Targets[0].ContainerMappings[0].Destination = "mutated"
	clone.Status.Members[0].Pod.UID = "mutated"
	clone.Status.Members[0].Containers[0].ContainerID = "mutated"
	clone.Status.Members[0].StartedAt.Time = time.Time{}
	clone.Status.Conditions[0].Reason = "Changed"
	if original.Spec.Targets[0].PodRef.Name != "llm-worker-0-r1" {
		t.Errorf("mutating clone target changed original: got %q", original.Spec.Targets[0].PodRef.Name)
	}
	if original.Spec.Targets[0].ContainerMappings[0].Destination != "server" {
		t.Errorf("mutating clone container mappings changed original: got %q",
			original.Spec.Targets[0].ContainerMappings[0].Destination)
	}
	if original.Status.Members[0].Containers[0].ContainerID != "containerd://c1" {
		t.Errorf("mutating clone containers changed original: got %q",
			original.Status.Members[0].Containers[0].ContainerID)
	}
	if original.Status.Members[0].Pod.UID != "pod-uid" {
		t.Errorf("mutating clone pod changed original: got %q", original.Status.Members[0].Pod.UID)
	}
	if !original.Status.Members[0].StartedAt.Equal(&startedAt) {
		t.Errorf("mutating clone startedAt changed original: got %v", original.Status.Members[0].StartedAt)
	}
	if original.Status.Conditions[0].Reason != "Released" {
		t.Errorf("mutating clone condition changed original: got %q", original.Status.Conditions[0].Reason)
	}
}

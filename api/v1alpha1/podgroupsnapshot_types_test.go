// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"reflect"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TestSchemeRegistersPodGroupSnapshotKinds verifies AddToScheme exposes
// PodGroupSnapshot and PodGroupSnapshotList.
func TestSchemeRegistersPodGroupSnapshotKinds(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme failed: %v", err)
	}
	for _, kind := range []string{KindPodGroupSnapshot, KindPodGroupSnapshot + "List"} {
		if !scheme.Recognizes(GroupVersion.WithKind(kind)) {
			t.Errorf("scheme does not recognize kind %q in %s", kind, GroupVersion.String())
		}
	}
}

// TestPodGroupSnapshotDeepCopyIsIndependent verifies the generated deepcopy
// produces an equal but independent PodGroupSnapshot, including through member
// sources and templates and the pointers in member statuses.
func TestPodGroupSnapshotDeepCopyIsIndependent(t *testing.T) {
	startedAt := metav1.NewTime(time.Date(2026, time.October, 16, 18, 2, 11, 0, time.UTC))
	original := &PodGroupSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-tp8", Namespace: "serving"},
		Spec: PodGroupSnapshotSpec{
			DeadlineSeconds: 1800,
			Members: []PodGroupSnapshotMember{{
				ID: "worker-0",
				Source: PodSnapshotSource{
					PodRef: PodReference{Name: "llm-worker-0", UID: "pod-uid-0", Containers: []string{"main"}},
				},
				PodSnapshotTemplate: &PodGroupMemberTemplate{
					Metadata: &PodSnapshotTemplateMetadata{Labels: map[string]string{"team": "inference"}},
					Containers: []ContainerSnapshotConfig{{
						Name: "main",
						CheckpointReadinessProbe: &CheckpointReadinessProbe{
							CheckpointReadinessProbeHandler: CheckpointReadinessProbeHandler{
								File: &ControlFileAction{Path: "/tmp/ready"},
							},
						},
					}},
				},
			}},
		},
		Status: PodGroupSnapshotStatus{
			Members: []PodGroupSnapshotMemberStatus{{
				ID:          "worker-0",
				NodeName:    "node-a",
				PodSnapshot: &ObjectIdentity{Name: "llm-tp8-worker-0", UID: "pod-snapshot-uid"},
				Content:     &ObjectIdentity{Name: "content-0", UID: "content-uid"},
				StartedAt:   &startedAt,
				Phase:       CheckpointPhaseCheckpointed,
			}},
			Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Checkpointed"}},
		},
	}

	clone := original.DeepCopy()
	if !reflect.DeepEqual(original, clone) {
		t.Fatalf("DeepCopy is not equal to original")
	}

	clone.Spec.Members[0].ID = "mutated"
	clone.Spec.Members[0].Source.PodRef.Containers[0] = "mutated-container"
	clone.Spec.Members[0].PodSnapshotTemplate.Metadata.Labels["team"] = "mutated"
	clone.Spec.Members[0].PodSnapshotTemplate.Containers[0].CheckpointReadinessProbe.File.Path = "/mutated"
	clone.Status.Members[0].PodSnapshot.Name = "mutated"
	clone.Status.Members[0].Content.UID = "mutated"
	clone.Status.Members[0].StartedAt.Time = time.Time{}
	clone.Status.Conditions[0].Reason = "Changed"
	if original.Spec.Members[0].ID != "worker-0" {
		t.Errorf("mutating clone member changed original: got %q", original.Spec.Members[0].ID)
	}
	if original.Spec.Members[0].Source.PodRef.Containers[0] != "main" {
		t.Errorf("mutating clone Containers slice changed original: got %q",
			original.Spec.Members[0].Source.PodRef.Containers[0])
	}
	template := original.Spec.Members[0].PodSnapshotTemplate
	if template.Metadata.Labels["team"] != "inference" {
		t.Errorf("mutating clone template labels changed original: got %q", template.Metadata.Labels["team"])
	}
	if path := template.Containers[0].CheckpointReadinessProbe.File.Path; path != "/tmp/ready" {
		t.Errorf("mutating clone template containers changed original: got %q", path)
	}
	if original.Status.Members[0].PodSnapshot.Name != "llm-tp8-worker-0" {
		t.Errorf("mutating clone podSnapshot changed original: got %q", original.Status.Members[0].PodSnapshot.Name)
	}
	if original.Status.Members[0].Content.UID != "content-uid" {
		t.Errorf("mutating clone content changed original: got %q", original.Status.Members[0].Content.UID)
	}
	if !original.Status.Members[0].StartedAt.Equal(&startedAt) {
		t.Errorf("mutating clone startedAt changed original: got %v", original.Status.Members[0].StartedAt)
	}
	if original.Status.Conditions[0].Reason != "Checkpointed" {
		t.Errorf("mutating clone condition changed original: got %q", original.Status.Conditions[0].Reason)
	}
}

// PodGroupMemberTemplate hands these settings to every member of a
// PodGroupSnapshot. The two types decide what Snapshot does to a member, and
// when; the probe, handler and action types only refine how, so they stay out.
func TestPodGroupMemberTemplateKnowsEverySetting(t *testing.T) {
	for typ, known := range map[reflect.Type][]string{
		reflect.TypeFor[ContainerSnapshotConfig](): {"Name", "CheckpointReadinessProbe", "Lifecycle"},
		reflect.TypeFor[SnapshotLifecycle]():       {"PostRestore"},
	} {
		var fields []string
		for i := range typ.NumField() {
			fields = append(fields, typ.Field(i).Name)
		}
		if !slices.Equal(fields, known) {
			t.Errorf("%s has fields %v, but this test knows %v. A new field reaches every member "+
				"of a PodGroupSnapshot through PodGroupMemberTemplate: define its place relative to "+
				"the group's barriers and list it here, or refuse it in PodGroupMemberTemplate "+
				"until it has one.", typ.Name(), fields, known)
		}
	}
}

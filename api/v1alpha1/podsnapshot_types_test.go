/*
 * SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package v1alpha1

import (
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// TestSchemeRegistersSnapshotKinds verifies the init() registrations expose all
// four new kinds through the package AddToScheme.
func TestSchemeRegistersSnapshotKinds(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme failed: %v", err)
	}
	for _, kind := range []string{"PodSnapshot", "PodSnapshotList", "PodSnapshotContent", "PodSnapshotContentList"} {
		if !scheme.Recognizes(GroupVersion.WithKind(kind)) {
			t.Errorf("scheme does not recognize kind %q in %s", kind, GroupVersion.String())
		}
	}
}

// TestSnapshotDeepCopyIsIndependent verifies the generated deepcopy produces an
// equal but independent PodSnapshot (mutating the clone must not touch the source).
func TestSnapshotDeepCopyIsIndependent(t *testing.T) {
	original := &PodSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-a", Namespace: "inference"},
		Spec: PodSnapshotSpec{
			Source: PodSnapshotSource{PodRef: &PodReference{Name: "worker-0", Containers: []string{"main"}}},
		},
		Status: PodSnapshotStatus{
			Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Captured"}},
		},
	}

	clone := original.DeepCopy()
	if !reflect.DeepEqual(original, clone) {
		t.Fatalf("DeepCopy is not equal to original")
	}

	clone.Spec.Source.PodRef.Name = "mutated"
	clone.Spec.Source.PodRef.Containers[0] = "mutated-container"
	clone.Status.Conditions[0].Reason = "Changed"
	if original.Spec.Source.PodRef.Name != "worker-0" {
		t.Errorf("mutating clone spec changed original: got %q", original.Spec.Source.PodRef.Name)
	}
	if original.Spec.Source.PodRef.Containers[0] != "main" {
		t.Errorf("mutating clone Containers slice changed original: got %q", original.Spec.Source.PodRef.Containers[0])
	}
	if original.Status.Conditions[0].Reason != "Captured" {
		t.Errorf("mutating clone condition changed original: got %q", original.Status.Conditions[0].Reason)
	}
}

// TestSnapshotContentDeepCopyIsIndependent verifies the generated deepcopy for
// the cluster-scoped PodSnapshotContent is equal but independent.
func TestSnapshotContentDeepCopyIsIndependent(t *testing.T) {
	original := &PodSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "content-a"},
		Spec: PodSnapshotContentSpec{
			PodSnapshotRef: PodSnapshotReference{Namespace: "inference", Name: "snap-a", UID: types.UID("uid-1")},
			Source: PodSnapshotContentSource{
				PodRef:   &PodReference{Name: "worker-0", UID: types.UID("pod-uid-1"), Containers: []string{"main"}},
				NodeName: "node-a",
			},
			Containers: []ContainerSnapshotConfig{{
				Name: "main",
				CheckpointReadinessProbe: &CheckpointReadinessProbe{CheckpointReadinessProbeHandler: CheckpointReadinessProbeHandler{
					File: &ControlFileAction{Path: "/tmp/ready"},
				}},
				Lifecycle: &SnapshotLifecycle{PostRestore: &PostRestoreAction{PostRestoreHandler: PostRestoreHandler{
					HTTPPost: &HTTPPostAction{Path: "/resume", Port: intstr.FromInt32(8000)},
				}}},
			}},
		},
		Status: PodSnapshotContentStatus{
			Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Bound"}},
			Source: &CheckpointSource{
				Node: &CheckpointSourceNode{Name: "node-a", Architecture: "amd64"},
				Pod:  &CheckpointSourcePod{Image: "example.com/worker:v1", Memory: "64Gi"},
				Devices: &CheckpointSourceDevices{
					Nvidia: &NvidiaCheckpointSource{
						DriverVersion: "580.82.07",
						Instances:     []NvidiaCheckpointSourceInstance{{ProductName: "NVIDIA A100-SXM4-80GB"}},
					},
				},
			},
		},
	}

	clone := original.DeepCopy()
	if !reflect.DeepEqual(original, clone) {
		t.Fatalf("DeepCopy is not equal to original")
	}

	clone.Spec.Source.PodRef.Name = "mutated"
	clone.Spec.Source.PodRef.Containers[0] = "mutated-container"
	clone.Status.Conditions[0].Reason = "Changed"
	clone.Status.Source.Node.Name = "node-b"
	clone.Status.Source.Pod.Memory = "32Gi"
	clone.Status.Source.Devices.Nvidia.Instances[0].ProductName = "NVIDIA H100"
	clone.Spec.Containers[0].CheckpointReadinessProbe.File.Path = "/tmp/mutated"
	clone.Spec.Containers[0].Lifecycle.PostRestore.HTTPPost.Port = intstr.FromInt32(9000)
	if original.Spec.Source.PodRef.Name != "worker-0" {
		t.Errorf("mutating clone changed original podRef name: got %q", original.Spec.Source.PodRef.Name)
	}
	if original.Spec.Source.PodRef.Containers[0] != "main" {
		t.Errorf("mutating clone Containers slice changed original: got %q", original.Spec.Source.PodRef.Containers[0])
	}
	if original.Status.Conditions[0].Reason != "Bound" {
		t.Errorf("mutating clone condition changed original: got %q", original.Status.Conditions[0].Reason)
	}
	if original.Status.Source.Node.Name != "node-a" {
		t.Errorf("mutating clone source node changed original: got %q", original.Status.Source.Node.Name)
	}
	if original.Status.Source.Pod.Memory != "64Gi" {
		t.Errorf("mutating clone source pod changed original: got %q", original.Status.Source.Pod.Memory)
	}
	if original.Status.Source.Devices.Nvidia.Instances[0].ProductName != "NVIDIA A100-SXM4-80GB" {
		t.Errorf("mutating clone source device changed original: got %q", original.Status.Source.Devices.Nvidia.Instances[0].ProductName)
	}
	if got := original.Spec.Containers[0].CheckpointReadinessProbe.File.Path; got != "/tmp/ready" {
		t.Errorf("mutating clone probe changed original: got %q", got)
	}
	if got := original.Spec.Containers[0].Lifecycle.PostRestore.HTTPPost.Port; got != intstr.FromInt32(8000) {
		t.Errorf("mutating clone post-restore action changed original: got %s", got.String())
	}
}

// TestArtifactUIDIsTheCapturedIdentity pins the one rule every artifact reader depends on: a
// capture's artifact is stored under the content's own UID, an import's under the UID it was
// captured with elsewhere, which is the one its manifest records.
func TestArtifactUIDIsTheCapturedIdentity(t *testing.T) {
	captured := &PodSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("local-uid")},
		Spec: PodSnapshotContentSpec{Source: PodSnapshotContentSource{
			PodRef: &PodReference{Name: "worker-0", Containers: []string{"main"}}, NodeName: "node-a",
		}},
	}
	if captured.IsImported() || captured.ArtifactUID() != "local-uid" {
		t.Errorf("capture: imported=%v artifact uid %q, want false and local-uid", captured.IsImported(), captured.ArtifactUID())
	}
	if got := captured.TargetContainers(); !reflect.DeepEqual(got, []string{"main"}) {
		t.Errorf("capture containers = %v", got)
	}

	imported := &PodSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("local-uid")},
		Spec: PodSnapshotContentSpec{Source: PodSnapshotContentSource{
			Artifact: &ArtifactReference{UID: "6b1e9f0c-2d4a-4c8e-9f3b-7a5d1e2c4b6a", Containers: []string{"engine"}},
		}},
	}
	if !imported.IsImported() || imported.ArtifactUID() != "6b1e9f0c-2d4a-4c8e-9f3b-7a5d1e2c4b6a" {
		t.Errorf("import: imported=%v artifact uid %q, want true and the captured uid", imported.IsImported(), imported.ArtifactUID())
	}
	if got := imported.TargetContainers(); !reflect.DeepEqual(got, []string{"engine"}) {
		t.Errorf("import containers = %v", got)
	}

	if got := (&PodSnapshotContent{}).TargetContainers(); got != nil {
		t.Errorf("no source: containers = %v, want nil", got)
	}
}

func TestPodSnapshotSourceTargetContainers(t *testing.T) {
	for name, tc := range map[string]struct {
		source PodSnapshotSource
		want   []string
	}{
		"pod":      {PodSnapshotSource{PodRef: &PodReference{Name: "w", Containers: []string{"main"}}}, []string{"main"}},
		"artifact": {PodSnapshotSource{Artifact: &ArtifactReference{UID: "u", Containers: []string{"engine"}}}, []string{"engine"}},
		"neither":  {PodSnapshotSource{}, nil},
	} {
		if got := tc.source.TargetContainers(); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: TargetContainers() = %v, want %v", name, got, tc.want)
		}
	}
}

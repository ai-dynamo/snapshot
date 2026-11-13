// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

const gangGate = "example.com/gang"

// groupTargetFixture returns an owner's Pod whose destination container,
// server, is not named after the checkpointed container, main.
func groupTargetFixture() *corev1.Pod {
	pod := restorePodFixture()
	pod.Spec.Containers[0].Name = "server"
	return pod
}

func toServer() GroupTargetRequest {
	return GroupTargetRequest{DestinationContainer: "server"}
}

// gatedGroupTarget returns the fixture as BuildGroupTarget shapes it.
func gatedGroupTarget(t *testing.T) *corev1.Pod {
	t.Helper()
	pod, err := BuildGroupTarget(groupTargetFixture(), toServer(), Options{})
	if err != nil {
		t.Fatalf("BuildGroupTarget() failed: %v", err)
	}
	return pod
}

func restoreGroupGates(pod *corev1.Pod) int {
	count := 0
	for _, gate := range pod.Spec.SchedulingGates {
		if gate.Name == RestoreGroupSchedulingGate {
			count++
		}
	}
	return count
}

func TestBuildGroupTargetShapesTheDestination(t *testing.T) {
	original := groupTargetFixture()
	before := original.DeepCopy()

	shaped, err := BuildGroupTarget(original, toServer(), Options{SeccompProfile: DefaultSeccompLocalhostProfile})
	if err != nil {
		t.Fatalf("BuildGroupTarget() failed: %v", err)
	}
	if !reflect.DeepEqual(original, before) {
		t.Fatal("BuildGroupTarget() mutated its input")
	}
	if !reflect.DeepEqual(shaped.Annotations, before.Annotations) {
		t.Fatalf("BuildGroupTarget() wrote annotations: %v", shaped.Annotations)
	}
	if got := restoreGroupGates(shaped); got != 1 || len(shaped.Spec.SchedulingGates) != 1 {
		t.Fatalf("scheduling gates = %v, want %s once", shaped.Spec.SchedulingGates, RestoreGroupSchedulingGate)
	}
	if !HasRestoreGroupGate(shaped) {
		t.Fatal("HasRestoreGroupGate() = false on a built group target")
	}
	if len(shaped.Spec.Volumes) != 1 || shaped.Spec.Volumes[0].EmptyDir == nil {
		t.Fatalf("expected one snapshot control emptyDir, got %#v", shaped.Spec.Volumes)
	}
	server := &shaped.Spec.Containers[0]
	if len(server.VolumeMounts) != 1 || server.VolumeMounts[0].SubPath != "server" {
		t.Fatalf("unexpected snapshot control mount: %#v", server.VolumeMounts)
	}
	for _, name := range []string{SnapshotControlDirEnv, LegacySnapshotControlDirEnv} {
		if got := restoreEnvValue(server.Env, name); got != SnapshotControlMountPath {
			t.Fatalf("%s = %q, want %q", name, got, SnapshotControlMountPath)
		}
	}
	if !reflect.DeepEqual(server.StartupProbe, canonicalRestoreStartupProbe()) {
		t.Fatalf("a group target lacks the restore startup probe: %#v", server.StartupProbe)
	}
	if server.SecurityContext == nil ||
		!matchesLocalhostSeccompProfile(server.SecurityContext.SeccompProfile, DefaultSeccompLocalhostProfile) {
		t.Fatalf("destination is missing the requested seccomp profile: %#v", server.SecurityContext)
	}
	if !reflect.DeepEqual(shaped.Spec.Containers[1], before.Spec.Containers[1]) {
		t.Fatal("non-destination sidecar was modified")
	}
	if err := ValidateGroupTarget(shaped, toServer()); err != nil {
		t.Fatalf("ValidateGroupTarget() rejected builder output: %v", err)
	}

	again, err := BuildGroupTarget(shaped, toServer(), Options{SeccompProfile: DefaultSeccompLocalhostProfile})
	if err != nil {
		t.Fatalf("second BuildGroupTarget() failed: %v", err)
	}
	if !reflect.DeepEqual(shaped, again) {
		t.Fatal("BuildGroupTarget() is not idempotent")
	}
}

func TestBuildGroupTargetAlwaysAddsTheStartupProbe(t *testing.T) {
	pod := groupTargetFixture()
	pod.Spec.Containers[0].StartupProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/startup"}},
	}
	shaped, err := BuildGroupTarget(pod, toServer(), Options{EnableStartupGate: false})
	if err != nil {
		t.Fatalf("BuildGroupTarget() failed: %v", err)
	}
	if !reflect.DeepEqual(shaped.Spec.Containers[0].StartupProbe, canonicalRestoreStartupProbe()) {
		t.Fatalf("startup probe = %#v, want the restore startup probe", shaped.Spec.Containers[0].StartupProbe)
	}
	if shaped.Spec.Containers[0].SecurityContext != nil {
		t.Fatal("an empty SeccompProfile changed the destination's security context")
	}
}

func TestBuildGroupTargetAddsTheGateOnce(t *testing.T) {
	pod := groupTargetFixture()
	pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: gangGate}, {Name: RestoreGroupSchedulingGate}}
	shaped, err := BuildGroupTarget(pod, toServer(), Options{})
	if err != nil {
		t.Fatalf("BuildGroupTarget() failed: %v", err)
	}
	if !reflect.DeepEqual(shaped.Spec.SchedulingGates, pod.Spec.SchedulingGates) {
		t.Fatalf("scheduling gates = %v, want %v", shaped.Spec.SchedulingGates, pod.Spec.SchedulingGates)
	}
}

func TestBuildGroupTargetRefusesAtomically(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*corev1.Pod)
		request GroupTargetRequest
		refusal string
	}{
		{
			name:    "restore-from",
			mutate:  func(pod *corev1.Pod) { pod.Annotations[RestoreFromAnnotation] = "snapshot-a" },
			request: toServer(),
			refusal: "must not carry " + RestoreFromAnnotation,
		},
		{
			name:    "an existing binding",
			mutate:  func(pod *corev1.Pod) { pod.Annotations[RestoreGroupUIDAnnotation] = "uid-r1" },
			request: toServer(),
			refusal: "must not carry " + RestoreGroupUIDAnnotation,
		},
		{
			name:    "an existing map",
			mutate:  func(pod *corev1.Pod) { pod.Annotations[RestoreContainerMapAnnotation] = "main=server" },
			request: toServer(),
			refusal: "must not carry " + RestoreContainerMapAnnotation,
		},
		{
			name:    "a missing destination",
			request: GroupTargetRequest{DestinationContainer: "missing"},
			refusal: "no destination container named",
		},
		{
			name:    "no destination",
			request: GroupTargetRequest{},
			refusal: "invalid restore destination container",
		},
		{
			name:    "an invalid destination",
			request: GroupTargetRequest{DestinationContainer: "Server"},
			refusal: "invalid restore destination container",
		},
		{
			name: "a conflicting control mount",
			mutate: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{
					Name: SnapshotControlVolumeName, MountPath: "/wrong", SubPath: "server",
				}}
			},
			request: toServer(),
			refusal: "requires volume",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := groupTargetFixture()
			if tc.mutate != nil {
				tc.mutate(pod)
			}
			before := pod.DeepCopy()

			_, err := BuildGroupTarget(pod, tc.request, Options{})
			if err == nil || !strings.Contains(err.Error(), tc.refusal) {
				t.Fatalf("BuildGroupTarget() error = %v, want one containing %q", err, tc.refusal)
			}
			if !reflect.DeepEqual(pod, before) {
				t.Fatal("BuildGroupTarget() mutated input after failure")
			}
		})
	}
}

func TestValidateGroupTarget(t *testing.T) {
	valid, err := BuildGroupTarget(groupTargetFixture(), toServer(),
		Options{SeccompProfile: DefaultSeccompLocalhostProfile})
	if err != nil {
		t.Fatalf("BuildGroupTarget() failed: %v", err)
	}

	for name, mutate := range map[string]func(*corev1.Pod){
		"as built": func(*corev1.Pod) {},
		"without the producer's seccomp profile": func(pod *corev1.Pod) {
			pod.Spec.Containers[0].SecurityContext = nil
		},
		"claimed, without the gate": func(pod *corev1.Pod) {
			pod.Spec.SchedulingGates = nil
			pod.Annotations[RestoreGroupUIDAnnotation] = "uid-r1"
			pod.Annotations[RestoreContainerMapAnnotation] = "main=server"
		},
	} {
		t.Run("accepts a Pod "+name, func(t *testing.T) {
			pod := valid.DeepCopy()
			mutate(pod)
			if err := ValidateGroupTarget(pod, toServer()); err != nil {
				t.Fatalf("ValidateGroupTarget() failed: %v", err)
			}
		})
	}

	for name, tc := range map[string]struct {
		mutate  func(*corev1.Pod)
		request GroupTargetRequest
	}{
		"no startup probe": {
			mutate: func(pod *corev1.Pod) { pod.Spec.Containers[0].StartupProbe = nil },
		},
		"another startup probe": {
			mutate: func(pod *corev1.Pod) { pod.Spec.Containers[0].StartupProbe.FailureThreshold = 7200 },
		},
		"no control volume": {
			mutate: func(pod *corev1.Pod) { pod.Spec.Volumes = nil },
		},
		"another control mount": {
			mutate: func(pod *corev1.Pod) { pod.Spec.Containers[0].VolumeMounts[0].SubPath = "other" },
		},
		"no control environment": {
			mutate: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].Env = removeRestoreEnv(pod.Spec.Containers[0].Env, SnapshotControlDirEnv)
			},
		},
		"another destination": {
			request: GroupTargetRequest{DestinationContainer: "sidecar"},
		},
		"a missing destination": {
			request: GroupTargetRequest{DestinationContainer: "missing"},
		},
		"an invalid destination": {
			request: GroupTargetRequest{DestinationContainer: "Server"},
		},
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			pod := valid.DeepCopy()
			if tc.mutate != nil {
				tc.mutate(pod)
			}
			request := tc.request
			if request == (GroupTargetRequest{}) {
				request = toServer()
			}
			if err := ValidateGroupTarget(pod, request); err == nil {
				t.Fatal("ValidateGroupTarget() unexpectedly succeeded")
			}
		})
	}
}

func TestClaimGroupTarget(t *testing.T) {
	claim := RestoreGroupClaim{UID: "uid-r1", Mapping: ContainerMapping{Source: "main", Destination: "server"}}

	t.Run("writes the claim and removes only its gate", func(t *testing.T) {
		gated := gatedGroupTarget(t)
		gated.Spec.SchedulingGates = append([]corev1.PodSchedulingGate{{Name: gangGate}}, gated.Spec.SchedulingGates...)
		before := gated.DeepCopy()

		claimed, err := ClaimGroupTarget(gated, claim)
		if err != nil {
			t.Fatalf("ClaimGroupTarget() failed: %v", err)
		}
		if !reflect.DeepEqual(gated, before) {
			t.Fatal("ClaimGroupTarget() mutated its input")
		}
		want := before.DeepCopy()
		want.Annotations[RestoreGroupUIDAnnotation] = "uid-r1"
		want.Annotations[RestoreContainerMapAnnotation] = "main=server"
		want.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: gangGate}}
		if !reflect.DeepEqual(claimed, want) {
			t.Fatalf("ClaimGroupTarget() changed more than the claim:\n got %#v\nwant %#v", claimed, want)
		}
		if HasRestoreGroupGate(claimed) {
			t.Fatal("HasRestoreGroupGate() = true on a claimed Pod")
		}
	})

	t.Run("writes the pair of a same-name mapping", func(t *testing.T) {
		pod, err := BuildGroupTarget(restorePodFixture(), GroupTargetRequest{DestinationContainer: "main"}, Options{})
		if err != nil {
			t.Fatalf("BuildGroupTarget() failed: %v", err)
		}
		pod.Annotations = nil
		claimed, err := ClaimGroupTarget(pod, RestoreGroupClaim{
			UID: "uid-r1", Mapping: ContainerMapping{Source: "main", Destination: "main"},
		})
		if err != nil {
			t.Fatalf("ClaimGroupTarget() failed: %v", err)
		}
		if got := claimed.Annotations[RestoreContainerMapAnnotation]; got != "main=main" {
			t.Fatalf("%s = %q, want main=main", RestoreContainerMapAnnotation, got)
		}
		if claimed.Spec.SchedulingGates != nil {
			t.Fatalf("scheduling gates = %v, want none", claimed.Spec.SchedulingGates)
		}
	})

	for _, tc := range []struct {
		name    string
		mutate  func(*corev1.Pod)
		claim   RestoreGroupClaim
		refusal string
	}{
		{
			name:    "no gate",
			mutate:  func(pod *corev1.Pod) { pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: gangGate}} },
			claim:   claim,
			refusal: "no " + RestoreGroupSchedulingGate + " scheduling gate",
		},
		{
			name:    "an existing binding",
			mutate:  func(pod *corev1.Pod) { pod.Annotations[RestoreGroupUIDAnnotation] = "uid-r0" },
			claim:   claim,
			refusal: "must not carry " + RestoreGroupUIDAnnotation,
		},
		{
			name:    "an existing map",
			mutate:  func(pod *corev1.Pod) { pod.Annotations[RestoreContainerMapAnnotation] = "main=server" },
			claim:   claim,
			refusal: "must not carry " + RestoreContainerMapAnnotation,
		},
		{
			name:    "restore-from",
			mutate:  func(pod *corev1.Pod) { pod.Annotations[RestoreFromAnnotation] = "snapshot-a" },
			claim:   claim,
			refusal: "must not carry " + RestoreFromAnnotation,
		},
		{
			name:    "an empty UID",
			claim:   RestoreGroupClaim{Mapping: claim.Mapping},
			refusal: "needs the UID",
		},
		{
			name:    "an invalid source",
			claim:   RestoreGroupClaim{UID: claim.UID, Mapping: ContainerMapping{Source: "Main", Destination: "server"}},
			refusal: "invalid restore source container",
		},
		{
			name:    "an invalid destination",
			claim:   RestoreGroupClaim{UID: claim.UID, Mapping: ContainerMapping{Source: "main", Destination: ""}},
			refusal: "invalid restore destination container",
		},
		{
			name:    "a missing destination",
			claim:   RestoreGroupClaim{UID: claim.UID, Mapping: ContainerMapping{Source: "main", Destination: "missing"}},
			refusal: "no destination container named",
		},
		{
			name:    "a destination without the control mount",
			mutate:  func(pod *corev1.Pod) { pod.Spec.Containers[0].VolumeMounts = nil },
			claim:   claim,
			refusal: "is missing " + SnapshotControlVolumeName,
		},
		{
			name:    "a destination without the startup probe",
			mutate:  func(pod *corev1.Pod) { pod.Spec.Containers[0].StartupProbe = nil },
			claim:   claim,
			refusal: "restore startup gate",
		},
	} {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			pod := gatedGroupTarget(t)
			if tc.mutate != nil {
				tc.mutate(pod)
			}
			before := pod.DeepCopy()
			_, err := ClaimGroupTarget(pod, tc.claim)
			if err == nil || !strings.Contains(err.Error(), tc.refusal) {
				t.Fatalf("ClaimGroupTarget() error = %v, want one containing %q", err, tc.refusal)
			}
			if !reflect.DeepEqual(pod, before) {
				t.Fatal("ClaimGroupTarget() mutated input after failure")
			}
		})
	}

	t.Run("refuses a nil Pod", func(t *testing.T) {
		if _, err := ClaimGroupTarget(nil, claim); err == nil {
			t.Fatal("ClaimGroupTarget(nil) unexpectedly succeeded")
		}
	})
}

func TestRestoreGroupClaimOf(t *testing.T) {
	claimed := func(uid, containerMap string) map[string]string {
		return map[string]string{RestoreGroupUIDAnnotation: uid, RestoreContainerMapAnnotation: containerMap}
	}

	t.Run("not claimed", func(t *testing.T) {
		for name, annotations := range map[string]map[string]string{
			"no annotations":       nil,
			"a standalone restore": {RestoreFromAnnotation: "snapshot-a", RestoreContainerMapAnnotation: "main=a,main=b"},
			"a map alone":          {RestoreContainerMapAnnotation: "main=server"},
		} {
			t.Run(name, func(t *testing.T) {
				claim, found, err := RestoreGroupClaimOf(annotations)
				if found || err != nil || claim != (RestoreGroupClaim{}) {
					t.Fatalf("RestoreGroupClaimOf() = %+v, %v, %v; want no claim", claim, found, err)
				}
			})
		}
	})

	t.Run("a claim", func(t *testing.T) {
		claim, found, err := RestoreGroupClaimOf(claimed("uid-r1", "main=server"))
		want := RestoreGroupClaim{UID: "uid-r1", Mapping: ContainerMapping{Source: "main", Destination: "server"}}
		if err != nil || !found || claim != want {
			t.Fatalf("RestoreGroupClaimOf() = %+v, %v, %v; want %+v", claim, found, err, want)
		}
	})

	withRestoreFrom := claimed("uid-r1", "main=server")
	withRestoreFrom[RestoreFromAnnotation] = "snapshot-a"
	for _, tc := range []struct {
		name        string
		annotations map[string]string
		refusal     string
	}{
		{"restore-from beside the binding", withRestoreFrom, "must not carry " + RestoreFromAnnotation},
		{"a binding without a map", map[string]string{RestoreGroupUIDAnnotation: "uid-r1"},
			"must carry " + RestoreContainerMapAnnotation},
		{"two pairs", claimed("uid-r1", "main=server,main=backup"), "exactly one pair, not 2"},
		{"an empty map", claimed("uid-r1", ""), "expected source=destination"},
		{"a pair without a destination", claimed("uid-r1", "main"), "expected source=destination"},
		{"an empty binding", claimed("", "main=server"), RestoreGroupUIDAnnotation + " is empty"},
		{"an invalid source", claimed("uid-r1", "Main=server"), "invalid restore source container"},
		{"an invalid destination", claimed("uid-r1", "main=Server"), "invalid restore destination container"},
	} {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			claim, found, err := RestoreGroupClaimOf(tc.annotations)
			if err == nil || !strings.Contains(err.Error(), tc.refusal) {
				t.Fatalf("RestoreGroupClaimOf() error = %v, want one containing %q", err, tc.refusal)
			}
			if !found {
				t.Fatal("a bound Pod with an invalid claim was reported as unclaimed")
			}
			if claim != (RestoreGroupClaim{}) {
				t.Fatalf("RestoreGroupClaimOf() returned %+v with its error", claim)
			}
		})
	}
}

func TestRestoreGroupClaimRoundTrip(t *testing.T) {
	for _, mapping := range []ContainerMapping{
		{Source: "main", Destination: "server"},
		{Source: "main", Destination: "main"},
	} {
		t.Run(mapping.String(), func(t *testing.T) {
			pod := restorePodFixture()
			pod.Spec.Containers[0].Name = mapping.Destination
			pod, err := BuildGroupTarget(pod, GroupTargetRequest{DestinationContainer: mapping.Destination}, Options{})
			if err != nil {
				t.Fatalf("BuildGroupTarget() failed: %v", err)
			}
			want := RestoreGroupClaim{UID: "uid-r1", Mapping: mapping}
			claimed, err := ClaimGroupTarget(pod, want)
			if err != nil {
				t.Fatalf("ClaimGroupTarget() failed: %v", err)
			}
			got, found, err := RestoreGroupClaimOf(claimed.Annotations)
			if err != nil || !found || got != want {
				t.Fatalf("RestoreGroupClaimOf(ClaimGroupTarget()) = %+v, %v, %v; want %+v", got, found, err, want)
			}
		})
	}
}

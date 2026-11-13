// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

func TestClassifyRestoreTarget(t *testing.T) {
	want := podcontract.RestoreGroupClaim{UID: "restore-uid", Mapping: podcontract.ContainerMapping{Source: "main", Destination: "main"}}
	gang := corev1.PodSchedulingGate{Name: "example.com/gang"}
	gated := func(mutate ...func(*corev1.Pod)) *corev1.Pod {
		pod := rgGatedPod("a")
		for _, m := range mutate {
			m(pod)
		}
		return pod
	}
	claimedBy := func(uid string, mutate ...func(*corev1.Pod)) *corev1.Pod {
		pod := rgClaim(rgGatedPod("a"), "restore-uid")
		pod.Annotations[podcontract.RestoreGroupUIDAnnotation] = uid
		for _, m := range mutate {
			m(pod)
		}
		return pod
	}
	withGate := func(gate corev1.PodSchedulingGate) func(*corev1.Pod) {
		return func(pod *corev1.Pod) { pod.Spec.SchedulingGates = append(pod.Spec.SchedulingGates, gate) }
	}
	withAnnotation := func(key, value string) func(*corev1.Pod) {
		return func(pod *corev1.Pod) {
			if pod.Annotations == nil {
				pod.Annotations = map[string]string{}
			}
			pod.Annotations[key] = value
		}
	}
	withoutAnnotation := func(key string) func(*corev1.Pod) {
		return func(pod *corev1.Pod) { delete(pod.Annotations, key) }
	}
	ungated := func(pod *corev1.Pod) { pod.Spec.SchedulingGates = nil }

	for _, tc := range []struct {
		name    string
		pod     *corev1.Pod
		want    restoreTargetClaim
		wantWhy string
	}{
		{"gated and unbound", gated(), restoreTargetUnclaimed, ""},
		{"gated and unbound, with a gang's gate too", gated(withGate(gang)), restoreTargetUnclaimed, ""},
		{"gated and unbound, with a container map its owner wrote", gated(withAnnotation(podcontract.RestoreContainerMapAnnotation, "main=main")),
			restoreTargetUnclaimed, ""},
		{"ungated, bound to this restore, with its map", claimedBy("restore-uid"), restoreTargetClaimed, ""},
		{"ungated, bound to this restore, with its map, and a gang's gate", claimedBy("restore-uid", withGate(gang)),
			restoreTargetClaimed, ""},
		{"bound to another UID", claimedBy("other-uid"), restoreTargetInUse,
			`is claimed by another PodGroupRestore, with UID "other-uid"`},
		{"bound to another UID, and gated again", claimedBy("other-uid", withGate(corev1.PodSchedulingGate{Name: podcontract.RestoreGroupSchedulingGate})),
			restoreTargetInUse, `is claimed by another PodGroupRestore, with UID "other-uid"`},
		{"ungated and unbound", gated(ungated), restoreTargetRefused,
			"is not a group target: it has neither the nvidia.com/restore-group scheduling gate nor a claim"},
		{"bound to this restore, with a different map",
			claimedBy("restore-uid", withAnnotation(podcontract.RestoreContainerMapAnnotation, "main=sidecar")), restoreTargetRefused,
			"is claimed by this PodGroupRestore with the container map main=sidecar, not main=main"},
		{"bound to this restore, and gated again",
			claimedBy("restore-uid", withGate(corev1.PodSchedulingGate{Name: podcontract.RestoreGroupSchedulingGate})), restoreTargetRefused,
			"is claimed by this PodGroupRestore but carries the nvidia.com/restore-group scheduling gate again"},
		{"bound to this restore, without a map", claimedBy("restore-uid", withoutAnnotation(podcontract.RestoreContainerMapAnnotation)),
			restoreTargetRefused, "carries an invalid claim: a claimed group target must carry nvidia.com/restore-container-map"},
		{"bound to another UID, without a map", claimedBy("other-uid", withoutAnnotation(podcontract.RestoreContainerMapAnnotation)),
			restoreTargetRefused, "carries an invalid claim: a claimed group target must carry nvidia.com/restore-container-map"},
		{"gated, and bound to this restore without a map", gated(withAnnotation(podcontract.RestoreGroupUIDAnnotation, "restore-uid")),
			restoreTargetRefused, "carries an invalid claim: a claimed group target must carry nvidia.com/restore-container-map"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := tc.pod.DeepCopy()
			got, why := classifyRestoreTarget(tc.pod, want)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.wantWhy, why)
			assert.Equal(t, before, tc.pod, "the claim table never changes a Pod")
		})
	}
}

func TestTargetsWaitNamesUnscheduledTargetsFirst(t *testing.T) {
	pending := podGroupAdmission{podGroupAdmissionWaiting, "TargetPodPending", "a pending"}
	unscheduled := podGroupAdmission{podGroupAdmissionWaiting, "TargetPodUnscheduled", "b unscheduled"}
	assert.Nil(t, targetsWait(nil))
	assert.Equal(t, &podGroupAdmission{podGroupAdmissionWaiting, "TargetPodPending", "a pending; a pending"},
		targetsWait([]podGroupAdmission{pending, pending}))
	assert.Equal(t, &podGroupAdmission{podGroupAdmissionWaiting, "TargetPodUnscheduled", "b unscheduled"},
		targetsWait([]podGroupAdmission{pending, unscheduled}))
}

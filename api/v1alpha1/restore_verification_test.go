// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIsRestoreVerifiedRejectsUnrelatedAndStaleResults(t *testing.T) {
	now := metav1.Now()
	p := &RestoreVerificationPolicy{PodRef: RestoreVerificationPodReference{Name: "canary", UID: "pod-1"}, ObservationWindowSeconds: 30, TimeoutSeconds: 120}
	s := &PodSnapshot{Spec: PodSnapshotSpec{RestoreVerification: p}, Status: PodSnapshotStatus{
		Phase:      PodSnapshotPhaseCompleted,
		Conditions: []metav1.Condition{{Type: PodSnapshotConditionReady, Status: metav1.ConditionTrue}},
		RestoreVerifyInfo: &RestoreVerificationInfo{ContentUID: "artifact-1", PolicyHash: RestoreVerificationPolicyHash(p), PodRef: p.PodRef,
			Outcome: RestoreVerificationSucceeded, CompletedAt: &now, Containers: []RestoreVerificationContainer{{Name: "main", ContainerID: "runtime://one"}}},
	}}
	require.True(t, IsRestoreVerified(s, "artifact-1"))
	for _, tc := range []struct {
		name   string
		change func(*PodSnapshot)
	}{
		{"disabled", func(s *PodSnapshot) { s.Spec.RestoreVerification = nil }},
		{"longer window", func(s *PodSnapshot) { s.Spec.RestoreVerification.ObservationWindowSeconds++ }},
		{"recreated pod", func(s *PodSnapshot) { s.Spec.RestoreVerification.PodRef.UID = "pod-2" }},
		{"capture failed", func(s *PodSnapshot) {
			s.Status.Conditions = append(s.Status.Conditions, metav1.Condition{Type: PodSnapshotConditionFailed, Status: metav1.ConditionTrue})
		}},
		{"verification pending", func(s *PodSnapshot) { s.Status.Phase = PodSnapshotPhaseRestoreVerifying }},
		{"missing execution", func(s *PodSnapshot) { s.Status.RestoreVerifyInfo.Containers = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := s.DeepCopy()
			tc.change(other)
			require.False(t, IsRestoreVerified(other, "artifact-1"))
		})
	}
	require.False(t, IsRestoreVerified(s, "artifact-2"))
}

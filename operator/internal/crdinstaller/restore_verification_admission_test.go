//go:build integration

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package crdinstaller_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	v1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/api/v1alpha1/crds"
)

func TestRestoreVerificationAdmission(t *testing.T) {
	var definitions []*apiextensionsv1.CustomResourceDefinition
	for _, raw := range []string{crds.PodSnapshotCRD(), crds.PodSnapshotContentCRD(), crds.SnapshotJobCRD()} {
		var definition apiextensionsv1.CustomResourceDefinition
		require.NoError(t, yaml.Unmarshal([]byte(raw), &definition))
		definitions = append(definitions, &definition)
	}
	environment := &envtest.Environment{CRDs: definitions}
	t.Cleanup(func() { assert.NoError(t, environment.Stop()) })
	config, err := environment.Start()
	require.NoError(t, err)
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	cl, err := client.New(config, client.Options{Scheme: scheme})
	require.NoError(t, err)
	ctx := t.Context()
	require.NoError(t, cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "workload"}}))
	for idx, tc := range []struct {
		name            string
		window, timeout int32
		uid             string
		valid           bool
	}{
		{"zero window", 0, 60, "pod-uid", true},
		{"window", 30, 120, "pod-uid", true},
		{"unbounded timeout", 0, 0, "pod-uid", false},
		{"negative window", -1, 60, "pod-uid", false},
		{"window exceeds timeout", 60, 60, "pod-uid", false},
		{"missing UID", 0, 60, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := fmt.Sprintf("verify-%d", idx)
			policy := &v1.RestoreVerificationPolicy{ObservationWindowSeconds: tc.window, TimeoutSeconds: tc.timeout}
			policy.PodRef.Name = "canary"
			policy.PodRef.UID = types.UID(tc.uid)
			s := newSettingsSnapshot(name, v1.ContainerSnapshotConfig{Name: "main"})
			s.Spec.RestoreVerification = policy
			err := cl.Create(ctx, s)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.True(t, apierrors.IsInvalid(err), "%v", err)
			}
			j := newSettingsJob(name, v1.ContainerSnapshotConfig{Name: "main"})
			j.Spec.PodSnapshotTemplate.RestoreVerification = policy
			err = cl.Create(ctx, j)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.True(t, apierrors.IsInvalid(err), "%v", err)
			}
			if !tc.valid {
				return
			}
			original := s.DeepCopy()
			s.Spec.RestoreVerification.PodRef.UID = "another-pod"
			require.True(t, apierrors.IsInvalid(cl.Update(ctx, s)), "target changes must preserve immutable spec")
			now := metav1.Now()
			original.Status.Phase = v1.PodSnapshotPhaseRestoreVerifying
			original.Status.RestoreVerifyInfo = &v1.RestoreVerificationInfo{ContentUID: "content-uid", PolicyHash: v1.RestoreVerificationPolicyHash(original.Spec.RestoreVerification), PodRef: original.Spec.RestoreVerification.PodRef, StartedAt: now, DeadlineAt: now, Outcome: v1.RestoreVerificationInProgress}
			require.NoError(t, cl.Status().Update(ctx, original))
			read := &v1.PodSnapshot{}
			require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(original), read))
			require.Equal(t, v1.PodSnapshotPhaseRestoreVerifying, read.Status.Phase)
			require.Equal(t, original.Status.RestoreVerifyInfo.PolicyHash, read.Status.RestoreVerifyInfo.PolicyHash)
		})
	}
}

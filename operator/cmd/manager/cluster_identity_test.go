// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ai-dynamo/snapshot/api/storage"
)

func TestKubeSystemUIDNamesTheCluster(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "11111111-2222-3333-4444-555555555555"}},
	).Build()

	id, err := kubeSystemUID(t.Context(), reader)

	require.NoError(t, err)
	assert.Equal(t, "11111111-2222-3333-4444-555555555555", id)
}

func TestKubeSystemUIDFailsWhenTheNamespaceCannotBeRead(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	_, err := kubeSystemUID(t.Context(), fake.NewClientBuilder().WithScheme(scheme).Build())

	assert.ErrorContains(t, err, "kube-system")
}

func TestMountedClaimsKnowsOnlyTheConfiguredClaim(t *testing.T) {
	mounted := mountedClaims(storage.Config{PVC: &storage.PVC{ClaimName: "snapshot-pvc"}}, "/checkpoints", nil)

	path, ok := mounted("snapshot-pvc")
	assert.True(t, ok)
	assert.Equal(t, "/checkpoints", path)
	_, ok = mounted("other")
	assert.False(t, ok)
}

func TestMountedClaimsKnowsNothingWithoutAPVCStore(t *testing.T) {
	_, ok := mountedClaims(storage.Config{}, "/checkpoints", nil)("snapshot-pvc")
	assert.False(t, ok)
}

func TestMountedClaimsPutsEachListedClaimBelowTheMountPath(t *testing.T) {
	mounted := mountedClaims(storage.Config{PVC: &storage.PVC{ClaimName: "snapshot-pvc"}}, "/checkpoints",
		[]string{"team-a", "team-b"})

	for claim, want := range map[string]string{
		"snapshot-pvc": "/checkpoints",
		"team-a":       "/checkpoints/.claims/team-a",
		"team-b":       "/checkpoints/.claims/team-b",
	} {
		got, ok := mounted(claim)
		assert.True(t, ok, claim)
		assert.Equal(t, want, got)
	}
	_, ok := mounted("team-c")
	assert.False(t, ok)
}

func TestMountedClaimsServesListedClaimsWithoutAPVCStore(t *testing.T) {
	mounted := mountedClaims(storage.Config{}, "/checkpoints", []string{"team-a"})

	got, ok := mounted("team-a")
	assert.True(t, ok)
	assert.Equal(t, "/checkpoints/.claims/team-a", got)
}

func TestParseClaimNames(t *testing.T) {
	names, err := parseClaimNames("team-a, team-b,,")
	require.NoError(t, err)
	assert.Equal(t, []string{"team-a", "team-b"}, names)

	names, err = parseClaimNames("")
	require.NoError(t, err)
	assert.Empty(t, names)

	for _, value := range []string{"team-a,team-a", "a/b", ".."} {
		_, err := parseClaimNames(value)
		assert.Error(t, err, value)
	}
}

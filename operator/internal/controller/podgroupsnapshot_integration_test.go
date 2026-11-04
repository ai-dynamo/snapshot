//go:build integration

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/api/v1alpha1/crds"
)

// TestPodGroupSnapshotRefusalFitsInStatus refuses the largest group with a
// refusal message of 32 KiB and records the status through the reconciler's
// own patch, against a real API server. Every member fails, so copying the
// refusal into each of them would put 8 MiB of messages in one object.
func TestPodGroupSnapshotRefusalFitsInStatus(t *testing.T) {
	cl := startPodGroupEnvtest(t)
	group := &snapshotv1alpha1.PodGroupSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("g", 63), Namespace: "default"},
		Spec:       snapshotv1alpha1.PodGroupSnapshotSpec{DeadlineSeconds: 600, Members: longestPodGroupMembers(256)},
	}
	require.NoError(t, cl.Create(t.Context(), group))

	var patchSize int
	patching := interceptor.NewClient(cl, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object,
			patch client.Patch, opts ...client.SubResourcePatchOption,
		) error {
			data, err := patch.Data(obj)
			if err != nil {
				return err
			}
			patchSize = len(data)
			return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
		},
	})
	r := &PodGroupSnapshotReconciler{Client: patching, Recorder: record.NewFakeRecorder(10)}

	refusal := strings.Repeat("x", 32<<10)
	now := time.Now()
	observed := podGroupSnapshotObservation{
		members:   make([]podGroupMemberObservation, len(group.Spec.Members)),
		admission: podGroupRefusal(refusal),
		deadline:  now.Add(time.Hour),
	}
	desired := derivePodGroupSnapshotStatus(&group.Spec, observed, group.Status, now)
	require.NoError(t, r.recordStatus(t.Context(), group, desired))
	t.Logf("status patch: %d bytes", patchSize)
	assert.Less(t, patchSize, 1<<20, "status patch bytes")

	stored := &snapshotv1alpha1.PodGroupSnapshot{}
	require.NoError(t, cl.Get(t.Context(), client.ObjectKeyFromObject(group), stored))
	failed := meta.FindStatusCondition(stored.Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, refusal, failed.Message, "the group's Failed condition keeps the refusal")
	require.Len(t, stored.Status.Members, len(group.Spec.Members))
	for _, m := range stored.Status.Members {
		assert.Equal(t, snapshotv1alpha1.CheckpointPhaseFailed, m.Phase, m.ID)
		assert.Equal(t, snapshotv1alpha1.PodGroupReasonRefused, m.Reason, m.ID)
		assert.Equal(t, podGroupRefusedMemberMessage, m.Message, m.ID)
	}
}

// startPodGroupEnvtest starts an API server with every embedded CRD installed.
func startPodGroupEnvtest(t *testing.T) client.WithWatch {
	t.Helper()
	var definitions []*apiextensionsv1.CustomResourceDefinition
	for _, manifest := range crds.All() {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		require.NoError(t, yaml.Unmarshal([]byte(manifest), crd))
		definitions = append(definitions, crd)
	}
	env := &envtest.Environment{CRDs: definitions}
	t.Cleanup(func() { assert.NoError(t, env.Stop()) })
	cfg, err := env.Start()
	require.NoError(t, err, "install the embedded CRDs")
	cl, err := client.NewWithWatch(cfg, client.Options{Scheme: snapshotReconcilerScheme()})
	require.NoError(t, err)
	return cl
}

// longestPodGroupMembers returns n members whose IDs and source Pod names are
// at their maximum lengths, the most each member adds to the group's status.
func longestPodGroupMembers(n int) []snapshotv1alpha1.PodGroupSnapshotMember {
	members := make([]snapshotv1alpha1.PodGroupSnapshotMember, n)
	for i := range members {
		id, pod := fmt.Sprintf("member-%d", i), fmt.Sprintf("pod-%d", i)
		members[i] = snapshotv1alpha1.PodGroupSnapshotMember{
			ID: id + strings.Repeat("x", 63-len(id)),
			Source: snapshotv1alpha1.PodSnapshotSource{PodRef: snapshotv1alpha1.PodReference{
				Name:       pod + strings.Repeat("x", 253-len(pod)),
				UID:        types.UID(fmt.Sprintf("source-uid-%d", i)),
				Containers: []string{"main"},
			}},
		}
	}
	return members
}

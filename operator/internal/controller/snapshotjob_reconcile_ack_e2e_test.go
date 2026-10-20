//go:build snapshot_e2e

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

func TestAcknowledgeE2EReconcileEmitsRequestedToken(t *testing.T) {
	recorder := record.NewFakeRecorder(1)
	reconciler := &SnapshotJobReconciler{Recorder: recorder}
	job := &snapshotv1alpha1.SnapshotJob{}
	job.Name = "job"
	job.Namespace = "namespace"
	job.UID = types.UID("job-uid")
	job.Annotations = map[string]string{
		snapshotJobReconcileAckAnnotation: "expected-token",
	}
	reconciler.acknowledgeE2EReconcile(job)

	event := <-recorder.Events
	require.Contains(t, event, "Normal SnapshotJobReconciled reconcile acknowledgment expected-token")
}

func TestSnapshotJobReconcileDoesNotAcknowledgeResourceErrors(t *testing.T) {
	scheme := snapshotJobReconcilerScheme()
	job := minimalSnapshotJob()
	job.Annotations = map[string]string{
		snapshotJobReconcileAckAnnotation: "error-token",
	}
	reconciler := makeSnapshotJobReconcilerWithInterceptor(
		scheme,
		interceptor.Funcs{
			Get: func(
				ctx context.Context,
				c client.WithWatch,
				key client.ObjectKey,
				obj client.Object,
				opts ...client.GetOption,
			) error {
				if _, ok := obj.(*batchv1.Job); ok {
					return errors.NewInternalError(fmt.Errorf("injected source Job read failure"))
				}
				return c.Get(ctx, key, obj, opts...)
			},
		},
		job,
	)

	_, err := reconciler.Reconcile(context.Background(), reconcileRequest(job))
	require.Error(t, err)
	select {
	case event := <-reconciler.Recorder.(*record.FakeRecorder).Events:
		t.Fatalf("unexpected reconciliation acknowledgment: %s", event)
	default:
	}
}

func TestSnapshotJobReconcileAcknowledgesAfterStatusPersistence(t *testing.T) {
	scheme := snapshotJobReconcilerScheme()
	job := minimalSnapshotJob()
	job.Annotations = map[string]string{
		snapshotJobReconcileAckAnnotation: "success-token",
	}
	events := record.NewFakeRecorder(2)
	statusPersisted := false
	reconciler := makeSnapshotJobReconcilerWithInterceptor(
		scheme,
		interceptor.Funcs{
			SubResourcePatch: func(
				ctx context.Context,
				c client.Client,
				subResourceName string,
				obj client.Object,
				patch client.Patch,
				opts ...client.SubResourcePatchOption,
			) error {
				if subResourceName == "status" {
					require.Empty(t, events.Events,
						"reconciliation acknowledgment must follow status persistence")
					statusPersisted = true
				}
				return c.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
			},
		},
		job,
	)
	reconciler.Recorder = events

	_, err := reconciler.Reconcile(context.Background(), reconcileRequest(job))
	require.NoError(t, err)
	require.True(t, statusPersisted)
	require.Contains(t, <-events.Events, "reconcile acknowledgment success-token")
}

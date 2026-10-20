//go:build snapshot_e2e

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

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

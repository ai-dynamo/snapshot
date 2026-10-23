//go:build snapshot_e2e

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	corev1 "k8s.io/api/core/v1"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

const snapshotJobReconcileAckAnnotation = "snapshot-e2e.nvidia.com/reconcile-ack"

func (r *SnapshotJobReconciler) acknowledgeE2EReconcile(sj *snapshotv1alpha1.SnapshotJob) {
	if token := sj.Annotations[snapshotJobReconcileAckAnnotation]; token != "" {
		r.Recorder.Eventf(
			sj,
			corev1.EventTypeNormal,
			"SnapshotJobReconciled",
			"reconcile acknowledgment %s",
			token,
		)
	}
}

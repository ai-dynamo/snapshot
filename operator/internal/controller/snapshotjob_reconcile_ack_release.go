//go:build !snapshot_e2e

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"

func (r *SnapshotJobReconciler) acknowledgeE2EReconcile(*snapshotv1alpha1.SnapshotJob) {}

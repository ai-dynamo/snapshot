// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

// Config is the resolved storage configuration the chart renders into
// storage.yaml for the operator and PageBroker. Exactly one backend block is
// populated and selects the backend; Helm's storage.type only chooses which
// block to render. S3 joins as a second block when that backend lands.
type Config struct {
	PVC *PVC `json:"pvc,omitempty" yaml:"pvc,omitempty"`
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

// Config is the resolved storage configuration the chart renders into
// storage.yaml for the operator. Exactly one backend block is populated and
// selects the backend; Helm's storage.type only chooses which block to
// render. PageBroker does not consume this file for the S3 backend: it reads
// its own resolved config, written separately with the computed store ID.
type Config struct {
	PVC *PVC `json:"pvc,omitempty" yaml:"pvc,omitempty"`
	S3  *S3  `json:"s3,omitempty" yaml:"s3,omitempty"`
}

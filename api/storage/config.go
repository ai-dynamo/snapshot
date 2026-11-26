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
	// S3Access holds the settings the default store needs besides the store
	// location. It is used only with S3.
	S3Access *S3Access `json:"s3Access,omitempty" yaml:"s3Access,omitempty"`
}

// S3Access holds the access settings of an S3 store. None of them identifies
// the store.
type S3Access struct {
	CredentialsSecretRef *KeyRef `json:"credentialsSecretRef,omitempty" yaml:"credentialsSecretRef,omitempty"`
	Addressing           string  `json:"addressing,omitempty" yaml:"addressing,omitempty"`
	AllowHTTP            bool    `json:"allowHttp,omitempty" yaml:"allowHttp,omitempty"`
	CABundleConfigMapRef *KeyRef `json:"caBundleConfigMapRef,omitempty" yaml:"caBundleConfigMapRef,omitempty"`
}

// KeyRef names a key of a Secret or ConfigMap in the Snapshot namespace.
type KeyRef struct {
	Name string `json:"name" yaml:"name"`
	Key  string `json:"key,omitempty" yaml:"key,omitempty"`
}

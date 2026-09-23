// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package types

import "fmt"

// DefaultS3CredentialsPath matches the operator manager's projected Secret mount.
const DefaultS3CredentialsPath = "/etc/snapshot/s3-credentials/credentials"

// S3Config configures the S3 maintenance backend. A nil *S3Config on
// ArtifactCleanupConfig means S3 maintenance is not registered.
type S3Config struct {
	Bucket          string
	Prefix          string
	Region          string
	Endpoint        string
	CredentialsPath string
	CABundlePath    string
}

func (c S3Config) Validate() error {
	if c.Bucket == "" {
		return fmt.Errorf("s3 bucket is required")
	}
	if c.Region == "" {
		return fmt.Errorf("s3 region is required")
	}
	if c.CredentialsPath == "" {
		return fmt.Errorf("s3 credentials path is required")
	}
	return nil
}

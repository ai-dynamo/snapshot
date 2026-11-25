// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package types

import "fmt"

// S3Config configures the S3 maintenance backend. A nil *S3Config on
// ArtifactCleanupConfig means S3 maintenance is not registered. An empty
// CredentialsPath selects the default credential chain under the operator's
// workload identity.
type S3Config struct {
	Bucket          string
	Prefix          string
	Region          string
	Endpoint        string
	Addressing      string
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
	return nil
}

// CredentialSource names where the S3 maintenance backend gets credentials.
func (c S3Config) CredentialSource() string {
	if c.CredentialsPath == "" {
		return "default credential chain"
	}
	return "credentials file"
}

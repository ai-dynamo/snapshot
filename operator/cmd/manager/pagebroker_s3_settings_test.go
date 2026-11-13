// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadPageBrokerS3SettingsEmptyPathKeepsPageBrokerDefaults(t *testing.T) {
	settings, err := loadPageBrokerS3Settings("")
	require.NoError(t, err)
	assert.Equal(t, pageBrokerS3Settings{}, settings)
}

func TestLoadPageBrokerS3Settings(t *testing.T) {
	staging := uint64(60129542144)
	retries := uint64(0)
	for _, tc := range []struct {
		name string
		yaml string
		want pageBrokerS3Settings
		err  string
	}{
		{
			name: "every member",
			yaml: "addressing: path\nallowHttp: true\ncaFile: /etc/snapshot-s3-ca/ca.crt\nlimits:\n  stagingBytes: 60129542144\n  uploadRequestRetries: 0\n",
			want: pageBrokerS3Settings{
				Addressing: "path",
				AllowHTTP:  true,
				CAFile:     "/etc/snapshot-s3-ca/ca.crt",
				Limits:     &pageBrokerS3Limits{StagingBytes: &staging, UploadRequestRetries: &retries},
			},
		},
		{name: "limits only", yaml: "limits:\n  stagingBytes: 60129542144\n", want: pageBrokerS3Settings{Limits: &pageBrokerS3Limits{StagingBytes: &staging}}},
		{name: "virtual addressing", yaml: "addressing: virtual\n", want: pageBrokerS3Settings{Addressing: "virtual"}},
		{name: "unknown addressing", yaml: "addressing: dual\n", err: "addressing must be path or virtual"},
		{name: "relative CA path", yaml: "caFile: ca.crt\n", err: "caFile must be an absolute path"},
		{name: "unknown limit", yaml: "limits:\n  uploadWorkers: 4\n", err: "decode PageBroker S3 settings"},
		{name: "unknown member", yaml: "storeId: x\n", err: "decode PageBroker S3 settings"},
		{name: "negative limit", yaml: "limits:\n  stagingBytes: -1\n", err: "decode PageBroker S3 settings"},
		{name: "malformed YAML", yaml: "[", err: "decode PageBroker S3 settings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "pagebroker-s3.yaml")
			require.NoError(t, os.WriteFile(path, []byte(tc.yaml), 0o600))
			settings, err := loadPageBrokerS3Settings(path)
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, settings)
		})
	}
}

func TestLoadPageBrokerS3SettingsMissingFile(t *testing.T) {
	_, err := loadPageBrokerS3Settings(filepath.Join(t.TempDir(), "missing.yaml"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

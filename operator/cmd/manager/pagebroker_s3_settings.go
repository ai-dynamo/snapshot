// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/yaml"
)

// pageBrokerS3Settings are the members of PageBroker's S3 configuration that
// are not store identity inputs. The chart renders them beside storage.yaml.
type pageBrokerS3Settings struct {
	Addressing string              `json:"addressing,omitempty"`
	AllowHTTP  bool                `json:"allowHttp,omitempty"`
	CAFile     string              `json:"caFile,omitempty"`
	Limits     *pageBrokerS3Limits `json:"limits,omitempty"`
}

// pageBrokerS3Limits mirrors PageBroker's "limits" object. PageBroker applies
// its own bounds and defaults to every member left out.
type pageBrokerS3Limits struct {
	TransactionSeconds   *uint64 `json:"transactionSeconds,omitempty"`
	StagingBytes         *uint64 `json:"stagingBytes,omitempty"`
	ActiveTransactions   *uint64 `json:"activeTransactions,omitempty"`
	UploadPartBytes      *uint64 `json:"uploadPartBytes,omitempty"`
	UploadBufferBytes    *uint64 `json:"uploadBufferBytes,omitempty"`
	UploadActiveFiles    *uint64 `json:"uploadActiveFiles,omitempty"`
	RequestSeconds       *uint64 `json:"requestSeconds,omitempty"`
	UploadRequestRetries *uint64 `json:"uploadRequestRetries,omitempty"`
	UploadConnectSeconds *uint64 `json:"uploadConnectSeconds,omitempty"`
}

// loadPageBrokerS3Settings reads the chart-rendered settings file. An empty
// path means none were rendered and PageBroker keeps its defaults.
func loadPageBrokerS3Settings(path string) (pageBrokerS3Settings, error) {
	if path == "" {
		return pageBrokerS3Settings{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return pageBrokerS3Settings{}, fmt.Errorf("read PageBroker S3 settings: %w", err)
	}
	var settings pageBrokerS3Settings
	if err := yaml.UnmarshalStrict(data, &settings); err != nil {
		return pageBrokerS3Settings{}, fmt.Errorf("decode PageBroker S3 settings: %w", err)
	}
	if err := settings.validate(); err != nil {
		return pageBrokerS3Settings{}, err
	}
	return settings, nil
}

func (s pageBrokerS3Settings) validate() error {
	switch s.Addressing {
	case "", "path", "virtual":
	default:
		return fmt.Errorf("PageBroker S3 addressing must be path or virtual, got %q", s.Addressing)
	}
	if s.CAFile != "" && !filepath.IsAbs(s.CAFile) {
		return fmt.Errorf("PageBroker S3 caFile must be an absolute path, got %q", s.CAFile)
	}
	return nil
}

// mustPageBrokerS3Settings loads the settings or exits with status 1.
func mustPageBrokerS3Settings(path string) pageBrokerS3Settings {
	settings, err := loadPageBrokerS3Settings(path)
	if err != nil {
		ctrl.Log.Error(err, "invalid PageBroker S3 settings")
		os.Exit(1)
	}
	return settings
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package maintenance

import (
	"context"
	"fmt"
	"strings"

	"github.com/ai-dynamo/snapshot/operator/internal/maintenance/backends"
	operatortypes "github.com/ai-dynamo/snapshot/operator/internal/types"
	"github.com/go-logr/logr"
)

// Backend performs maintenance storage operations for one configured store.
type Backend interface {
	Name() string
	// Delete is a no-op when contentUID has nothing to remove.
	Delete(ctx context.Context, contentUID string) error
	Candidates(ctx context.Context, logger logr.Logger) (map[string]struct{}, error)
}

// BackendRegistry looks up a Backend by Name(), case-insensitively.
type BackendRegistry struct {
	backends map[string]Backend
}

func (r *BackendRegistry) Init(ctx context.Context, cfg operatortypes.ArtifactCleanupConfig) error {
	r.register(backends.NewPVCBackend(cfg.BasePath))
	if cfg.S3 != nil {
		s3Backend, err := backends.NewS3Backend(ctx, backends.S3Config{
			Bucket:          cfg.S3.Bucket,
			Prefix:          cfg.S3.Prefix,
			Region:          cfg.S3.Region,
			Endpoint:        cfg.S3.Endpoint,
			CredentialsPath: cfg.S3.CredentialsPath,
			CABundlePath:    cfg.S3.CABundlePath,
		})
		if err != nil {
			return fmt.Errorf("construct S3 maintenance backend: %w", err)
		}
		r.register(s3Backend)
	}
	return nil
}

func (r *BackendRegistry) register(backend Backend) {
	if r.backends == nil {
		r.backends = make(map[string]Backend)
	}
	r.backends[strings.ToUpper(backend.Name())] = backend
}

func (r *BackendRegistry) Get(name string) (Backend, bool) {
	backend, ok := r.backends[strings.ToUpper(name)]
	return backend, ok
}

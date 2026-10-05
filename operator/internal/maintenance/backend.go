// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package maintenance

import (
	"context"
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

func (r *BackendRegistry) Init(cfg operatortypes.ArtifactCleanupConfig) {
	r.register(backends.NewPVCBackend(cfg.BasePath))
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

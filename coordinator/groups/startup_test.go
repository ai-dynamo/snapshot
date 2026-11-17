// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package groups

import (
	"context"
	"errors"
	"testing"

	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// listFailingCache is a synced cache whose every List fails with err.
type listFailingCache struct {
	cache.Cache
	err error
}

func (c listFailingCache) WaitForCacheSync(context.Context) bool { return true }

func (c listFailingCache) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return c.err
}

// TestStartupReturnsAFailedList checks that a List of the group resources that
// fails stops the startup with its error, and leaves the coordinator unready.
func TestStartupReturnsAFailedList(t *testing.T) {
	listErr := errors.New("no such index")
	srv := &server.Server{}
	s := &startup{cache: listFailingCache{err: listErr}, server: srv, log: logr.Discard()}
	require.ErrorIs(t, s.Start(t.Context()), listErr)
	assert.False(t, srv.Ready())
}

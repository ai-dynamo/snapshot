// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStoreDir covers the directory of the control buckets: it is removed with
// the server, a directory a stopped process left behind is removed when the
// next one starts, and a directory another process holds is not.
func TestStoreDir(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	exists := func(path string) bool {
		_, err := os.Stat(path)
		return err == nil
	}
	leftover := func() string {
		path, err := os.MkdirTemp("", storeDirPrefix)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(path, "jetstream"), []byte("x"), 0o600))
		return path
	}

	t.Run("a stopped process's directory is removed at startup", func(t *testing.T) {
		withLock, withoutLock := leftover(), leftover()
		// A crashed process leaves its lock file behind, with nobody holding it.
		lock, err := lockDir(withLock)
		require.NoError(t, err)
		require.NoError(t, lock.Close())
		d, err := newStoreDir(logr.Discard())
		require.NoError(t, err)
		t.Cleanup(d.remove)
		assert.False(t, exists(withLock))
		assert.False(t, exists(withoutLock))
		assert.True(t, exists(d.path), "the new directory must stay")
	})

	t.Run("a directory another process holds stays", func(t *testing.T) {
		first, err := newStoreDir(logr.Discard())
		require.NoError(t, err)
		t.Cleanup(first.remove)
		second, err := newStoreDir(logr.Discard())
		require.NoError(t, err)
		t.Cleanup(second.remove)
		assert.NotEqual(t, first.path, second.path)
		assert.True(t, exists(first.path), "a running coordinator's directory must stay")
	})

	t.Run("anything else under the temporary directory stays", func(t *testing.T) {
		other := filepath.Join(os.TempDir(), "other")
		require.NoError(t, os.Mkdir(other, 0o700))
		file := filepath.Join(os.TempDir(), storeDirPrefix+"file")
		require.NoError(t, os.WriteFile(file, nil, 0o600))
		d, err := newStoreDir(logr.Discard())
		require.NoError(t, err)
		t.Cleanup(d.remove)
		assert.True(t, exists(other))
		assert.True(t, exists(file))
	})

	t.Run("removing the directory releases it", func(t *testing.T) {
		d, err := newStoreDir(logr.Discard())
		require.NoError(t, err)
		d.remove()
		assert.False(t, exists(d.path))
	})
}

// TestShutdownRemovesTheStore checks that a server's file store does not
// outlive it.
func TestShutdownRemovesTheStore(t *testing.T) {
	h := newHarness(t)
	h.ensure(1)
	path := h.srv.store.path
	_, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, h.srv.Shutdown(within(t)))
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}

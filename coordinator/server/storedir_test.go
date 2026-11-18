// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ai-dynamo/snapshot/coordinator/internal/testcert"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/stretchr/testify/require"
)

// TestStoreDir covers the directory of the control buckets, which the server
// owns: a configured one is emptied, or created, when the server starts, and
// removed when it shuts down, and with none, the server makes a new one under
// the temporary directory, and removes it.
func TestStoreDir(t *testing.T) {
	t.Run("a configured directory is emptied, then removed", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "store")
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "jetstream", "stale"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "leftover"), []byte("x"), 0o600))
		h := newHarness(t, func(cfg *Config) { cfg.StoreDir = dir })
		h.ensure(1)
		require.NoFileExists(t, filepath.Join(dir, "leftover"))
		require.NoDirExists(t, filepath.Join(dir, "jetstream", "stale"))
		require.DirExists(t, filepath.Join(dir, "jetstream"), "the file store must live there")
		require.NoError(t, h.srv.Shutdown(within(t)))
		require.NoDirExists(t, dir)
	})

	t.Run("a configured directory that does not exist is created", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "parent", "store")
		h := newHarness(t, func(cfg *Config) { cfg.StoreDir = dir })
		h.ensure(1)
		require.DirExists(t, filepath.Join(dir, "jetstream"))
	})

	t.Run("with none, a new temporary directory, then removed", func(t *testing.T) {
		t.Setenv("TMPDIR", t.TempDir())
		h := newHarness(t)
		h.ensure(1)
		dir := h.srv.storeDir
		require.True(t, strings.HasPrefix(dir, filepath.Join(os.TempDir(), storeDirPrefix)), dir)
		require.DirExists(t, filepath.Join(dir, "jetstream"))
		require.NoError(t, h.srv.Shutdown(within(t)))
		require.NoDirExists(t, dir)
	})

	t.Run("a directory that cannot be made fails New", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "file")
		require.NoError(t, os.WriteFile(file, nil, 0o600))
		_, cert := testcert.New(t)
		_, err := New(Config{
			Host: "127.0.0.1", Port: natsserver.RANDOM_PORT, Authorizer: &testAuthorizer{},
			TLS:      &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
			StoreDir: filepath.Join(file, "store"),
		})
		require.Error(t, err)
	})
}

func TestStoreDirRefusesSymlinks(t *testing.T) {
	for name, suffix := range map[string]string{"bare": "", "slash": "/", "dot": "/."} {
		t.Run(name, func(t *testing.T) {
			target := t.TempDir()
			marker := filepath.Join(target, "keep")
			require.NoError(t, os.WriteFile(marker, []byte("keep"), 0o600))
			link := filepath.Join(t.TempDir(), "store")
			require.NoError(t, os.Symlink(target, link))
			_, err := newStoreDir(link + suffix)
			require.Error(t, err)
			require.ErrorContains(t, err, "symlink")
			value, err := os.ReadFile(marker)
			require.NoError(t, err)
			require.Equal(t, "keep", string(value))
		})
	}
}

func TestStoreDirRefusesAnotherOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("changing a directory's owner requires root")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "keep")
	require.NoError(t, os.WriteFile(marker, []byte("keep"), 0o600))
	require.NoError(t, os.Chown(dir, 1, -1))
	_, err := newStoreDir(dir)
	require.Error(t, err)
	require.ErrorContains(t, err, "owner")
	require.FileExists(t, marker)
}

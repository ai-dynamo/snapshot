// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/internal/testcert"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
)

// The lines waitForCertificate logs.
const (
	waitingLine = "Waiting for the serving certificate"
	foundLine   = "Serving certificate found"
)

// TestWaitForCertificate covers how Run reads its serving certificate: at once
// when its files exist, after waiting for both when they do not, and not at all
// when they do not parse or ctx ends first.
func TestWaitForCertificate(t *testing.T) {
	const interval = 10 * time.Millisecond

	// result is what waitForCertificate returns.
	type result struct {
		certs *certwatcher.CertWatcher
		err   error
	}
	// waiting starts waitForCertificate on dir and, once it has logged that it
	// waits, returns where its result arrives and the cancel of its ctx.
	waiting := func(t *testing.T, dir string, logs *logLines) (<-chan result, context.CancelFunc) {
		t.Helper()
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		done := make(chan result, 1)
		go func() {
			certs, err := waitForCertificate(ctx, dir, interval, logs.logger())
			done <- result{certs, err}
		}()
		require.Eventually(t, func() bool { return count(logs.all(), waitingLine) == 1 },
			10*time.Second, interval)
		return done, cancel
	}

	t.Run("files that exist", func(t *testing.T) {
		dir := t.TempDir()
		_, cert := testcert.New(t)
		writeCert(t, dir, cert)
		logs := &logLines{}
		certs, err := waitForCertificate(t.Context(), dir, interval, logs.logger())
		require.NoError(t, err)
		runUntilCleanup(t, certs)
		served, err := certs.GetCertificate(nil)
		require.NoError(t, err)
		assert.Equal(t, cert.Certificate[0], served.Certificate[0])
		assert.Empty(t, logs.all(), "no wait")
	})

	t.Run("files that appear one at a time", func(t *testing.T) {
		dir, staged := t.TempDir(), t.TempDir()
		_, cert := testcert.New(t)
		writeCert(t, staged, cert)
		logs := &logLines{}
		move := func(name string) {
			require.NoError(t, os.Rename(filepath.Join(staged, name), filepath.Join(dir, name)))
		}
		done, _ := waiting(t, dir, logs)
		move(corev1.TLSCertKey)
		select {
		case r := <-done:
			t.Fatalf("waitForCertificate returned without the key: %v", r.err)
		case <-time.After(10 * interval):
		}
		move(corev1.TLSPrivateKeyKey)
		r := <-done
		require.NoError(t, r.err)
		runUntilCleanup(t, r.certs)
		served, err := r.certs.GetCertificate(nil)
		require.NoError(t, err)
		assert.Equal(t, cert.Certificate[0], served.Certificate[0])
		lines := logs.all()
		assert.Equal(t, 1, count(lines, waitingLine), "logged once")
		assert.Equal(t, 1, count(lines, foundLine), "logged once")
		assert.Len(t, lines, 2)
	})

	t.Run("files that do not parse", func(t *testing.T) {
		dir := t.TempDir()
		for _, name := range []string{corev1.TLSCertKey, corev1.TLSPrivateKeyKey} {
			require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("not PEM"), 0o600))
		}
		_, err := waitForCertificate(t.Context(), dir, interval, logr.Discard())
		require.ErrorContains(t, err, "read the serving certificate")
	})

	t.Run("ctx ends first", func(t *testing.T) {
		logs := &logLines{}
		done, cancel := waiting(t, t.TempDir(), logs)
		cancel()
		r := <-done
		require.ErrorIs(t, r.err, context.Canceled)
		assert.Equal(t, 0, count(logs.all(), foundLine))
	})
}

// runUntilCleanup runs certs, as Run does, until the test ends:
// certwatcher.New opens a file watcher that only the end of Start's ctx
// closes. The cleanup waits for Start to return, before the certificate's
// temporary directory is removed.
func runUntilCleanup(t *testing.T, certs *certwatcher.CertWatcher) {
	t.Helper()
	started := make(chan error, 1)
	go func() { started <- certs.Start(t.Context()) }()
	t.Cleanup(func() { assert.NoError(t, <-started) })
}

// count returns how many of lines contain s.
func count(lines []string, s string) int {
	n := 0
	for _, line := range lines {
		if strings.Contains(line, s) {
			n++
		}
	}
	return n
}

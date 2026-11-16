// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

// runtimeLog records what controller-runtime's packages log, which they send
// to its global logger.
var runtimeLog = &logLines{}

// TestMain gives controller-runtime its global logger before any test runs:
// controller-runtime keeps the first one it is given.
func TestMain(m *testing.M) {
	ctrllog.SetLogger(runtimeLog.logger())
	os.Exit(m.Run())
}

// probe returns the status of a GET of path from the health endpoint at
// addr, or 0 if it does not answer.
func probe(addr, path string) int {
	c := http.Client{Timeout: time.Second}
	resp, err := c.Get("http://" + addr + path)
	if err != nil {
		return 0
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// writeCert writes cert and its key to dir as tls.crt and tls.key, as the
// volume of a kubernetes.io/tls Secret holds them.
func writeCert(t *testing.T, dir string, cert tls.Certificate) {
	t.Helper()
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, corev1.TLSCertKey),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, corev1.TLSPrivateKeyKey),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0o600))
}

// freePort returns a port of 127.0.0.1 that nothing listens on.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// logLines records every line the coordinator logs, at every verbosity,
// which never holds a key, a value, a barrier name, or a token. The test never
// prints them.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logger() logr.Logger {
	return funcr.New(func(prefix, args string) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.lines = append(l.lines, prefix+" "+args)
	}, funcr.Options{Verbosity: 9})
}

func (l *logLines) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.lines)
}

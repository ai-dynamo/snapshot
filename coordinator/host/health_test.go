// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/coordinatortest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHealthEndpoint covers the health endpoint: live while it serves, ready
// once the coordinator it reports admits clients, with nothing logged while
// it is not, and closed once stopped.
func TestHealthEndpoint(t *testing.T) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort(t)))
	h, err := serveHealth(addr)
	require.NoError(t, err)
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = h.stop()
		}
	})

	_, err = serveHealth(addr)
	require.ErrorContains(t, err, "listen for health probes", "a port in use")

	assert.Equal(t, 90*time.Second, h.srv.IdleTimeout, "http.DefaultTransport's idle timeout")
	assert.Equal(t, 5*time.Second, h.srv.ReadHeaderTimeout)

	assert.Equal(t, http.StatusOK, probe(addr, "/healthz"))
	c := coordinatortest.StartUnready(t)
	logged := len(runtimeLog.all())
	assert.Equal(t, http.StatusInternalServerError, probe(addr, "/readyz"), "no coordinator")
	h.report(c.Server)
	assert.Equal(t, http.StatusInternalServerError, probe(addr, "/readyz"), "a coordinator not ready")
	assert.Equal(t, logged, len(runtimeLog.all()), "a failed probe logs nothing")
	c.MarkReady()
	assert.Equal(t, http.StatusOK, probe(addr, "/readyz"))
	assert.Equal(t, http.StatusOK, probe(addr, "/healthz"))

	require.NoError(t, h.stop())
	stopped = true
	assert.Equal(t, 0, probe(addr, "/healthz"), "closed")
}

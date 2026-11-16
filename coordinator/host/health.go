// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/server"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// healthHeaderTimeout bounds how long the health endpoint waits for a
// request's headers.
const healthHeaderTimeout = 5 * time.Second

// errNotReady is the readiness check's failure.
var errNotReady = errors.New("the coordinator does not admit clients yet")

// healthEndpoint is the health endpoint the Pod's probes read. /healthz is ok
// while it serves, and /readyz once the coordinator it reports admits clients.
// The operator holds new groups on a node whose agent is not ready, so the Pod
// reports ready only then. A failed probe logs nothing, since the kubelet
// probes every few seconds for as long as the coordinator is not ready.
type healthEndpoint struct {
	srv         *http.Server
	served      chan error
	coordinator atomic.Pointer[server.Server]
}

// serveHealth serves the health endpoint on addr until stop.
func serveHealth(addr string) (*healthEndpoint, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen for health probes: %w", err)
	}
	h := &healthEndpoint{served: make(chan error, 1)}
	mux := http.NewServeMux()
	mux.Handle("/healthz", healthz.CheckHandler{Checker: healthz.Ping})
	mux.Handle("/readyz", healthz.CheckHandler{Checker: h.ready})
	h.srv = &http.Server{Handler: mux, ReadHeaderTimeout: healthHeaderTimeout}
	go func() { h.served <- h.srv.Serve(l) }()
	return h, nil
}

// report makes /readyz report whether srv admits clients.
func (h *healthEndpoint) report(srv *server.Server) { h.coordinator.Store(srv) }

// ready is the readiness check.
func (h *healthEndpoint) ready(*http.Request) error {
	if srv := h.coordinator.Load(); srv == nil || !srv.Ready() {
		return errNotReady
	}
	return nil
}

// stop closes the endpoint. It reports why the endpoint stopped serving
// before, if it did.
func (h *healthEndpoint) stop() error {
	if err := h.srv.Close(); err != nil {
		return fmt.Errorf("close the health endpoint: %w", err)
	}
	if err := <-h.served; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve the health endpoint: %w", err)
	}
	return nil
}

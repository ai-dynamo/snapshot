// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package coordinatortest runs a Snapshot coordinator in the tests of the
// session client's users, as net/http/httptest runs an HTTP server: in the
// test's own process, on a free port of 127.0.0.1, with a throwaway CA, and an
// Authorizer that admits Token.
package coordinatortest

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"sync/atomic"
	"testing"

	"github.com/ai-dynamo/snapshot/coordinator"
	"github.com/ai-dynamo/snapshot/coordinator/internal/testcert"
	"github.com/ai-dynamo/snapshot/coordinator/server"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"k8s.io/apimachinery/pkg/types"
)

// Token is the token an Authorizer admits.
const Token = "member-token"

// Authorizer admits Token for any member of any attempt, and refuses any other
// token, unless Unavailable is set.
type Authorizer struct {
	// Unavailable makes Authorize report server.ErrUnavailable for every token,
	// as an Authorizer whose TokenReviews fail does, so members wait to be
	// admitted.
	Unavailable atomic.Bool
}

// Authorize implements server.Authorizer.
func (a *Authorizer) Authorize(_ context.Context, _ types.UID, _, token string) error {
	switch {
	case a.Unavailable.Load():
		return server.ErrUnavailable
	case token != Token:
		return server.ErrUnauthenticated
	}
	return nil
}

// Coordinator is a coordinator running for a test.
type Coordinator struct {
	*server.Server

	// RootCAs verifies the coordinator's serving certificate.
	RootCAs *x509.CertPool

	// Authorizer is the coordinator's, which a test may make unavailable.
	Authorizer *Authorizer
}

// Start starts a coordinator, configured by modify, marks it ready, and shuts
// it down when the test ends. It listens on a free port of 127.0.0.1, with a
// throwaway certificate and a new Authorizer, which modify cannot replace. The
// test then ensures the sessions its members join.
func Start(t testing.TB, modify ...func(*server.Config)) *Coordinator {
	t.Helper()
	roots, cert := testcert.New(t)
	auth := &Authorizer{}
	cfg := server.Config{Host: "127.0.0.1", Port: natsserver.RANDOM_PORT}
	for _, m := range modify {
		m(&cfg)
	}
	cfg.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	cfg.Authorizer = auth
	srv, err := server.New(cfg)
	if err != nil {
		t.Fatalf("start the coordinator: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Shutdown(context.Background()); err != nil {
			t.Errorf("shut the coordinator down: %v", err)
		}
	})
	srv.MarkReady()
	return &Coordinator{Server: srv, RootCAs: roots, Authorizer: auth}
}

// Config returns the Config that connects member of the attempt uid to the
// coordinator, presenting Token.
func (c *Coordinator) Config(uid types.UID, member string) coordinator.Config {
	return coordinator.Config{
		URLs:       []string{"tls://" + c.Addr().String()},
		RootCAs:    func() (*x509.CertPool, error) { return c.RootCAs, nil },
		ServerName: "localhost",
		Attempt:    string(uid),
		Member:     member,
		Token:      func() (string, error) { return Token, nil },
	}
}

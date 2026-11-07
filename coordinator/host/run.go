// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/coordinator/agentauth"
	"github.com/ai-dynamo/snapshot/coordinator/groups"
	"github.com/ai-dynamo/snapshot/coordinator/server"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

const (
	// issuerTimeout bounds agentauth.New's reads of the API server's
	// ServiceAccount issuer and its keys, which nothing else bounds. Run fails
	// if they do not finish in time, and the container restarts.
	issuerTimeout = 30 * time.Second

	// shutdownTimeout bounds server.Shutdown, once the manager has stopped:
	// failing every open session, and waiting briefly for their members to
	// close their connections.
	shutdownTimeout = 5 * time.Second
)

// errNotReady is the readiness check's failure.
var errNotReady = errors.New("the coordinator does not admit clients yet")

// Run runs the coordinator that cfg configures until ctx is done, then shuts it
// down within a bounded time, failing every open session with
// session.ReasonCoordinatorLost. It builds the parts in the order package groups
// gives, and returns an error, having shut down what it started, if any of them
// fails. It returns nil once ctx is done, unless the manager or the shutdown
// fails.
func Run(ctx context.Context, cfg Config) (err error) {
	if err := cfg.validate(); err != nil {
		return err
	}
	mgr, err := newManager(cfg)
	if err != nil {
		return err
	}
	attempts, err := groups.NewAttempts(ctx, mgr)
	if err != nil {
		return err
	}
	auth, err := newAuthorizer(ctx, cfg, attempts)
	if err != nil {
		return err
	}
	certs, err := certwatcher.New(filepath.Join(cfg.TLSDir, corev1.TLSCertKey),
		filepath.Join(cfg.TLSDir, corev1.TLSPrivateKeyKey))
	if err != nil {
		return fmt.Errorf("read the serving certificate: %w", err)
	}
	if err := mgr.Add(certs); err != nil {
		return fmt.Errorf("watch the serving certificate: %w", err)
	}
	srv, err := server.New(server.Config{
		Host:            cfg.Host,
		Port:            cfg.Port,
		TLS:             &tls.Config{GetCertificate: certs.GetCertificate, MinVersion: tls.VersionTLS12},
		Authorizer:      auth,
		MaxBytes:        cfg.MaxBytes,
		MaxValue:        cfg.MaxValue,
		MaxLibraryValue: cfg.MaxLibraryValue,
		MaxLibraryKeys:  cfg.MaxLibraryKeys,
		Log:             cfg.Log.WithName("coordinator"),
	})
	if err != nil {
		return fmt.Errorf("start the coordinator: %w", err)
	}
	defer func() { err = errors.Join(err, shutdown(srv)) }()
	if err := groups.Setup(mgr, attempts, groups.Config{Server: srv, Node: cfg.Node}); err != nil {
		return err
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("add the liveness check: %w", err)
	}
	// The operator holds new groups on a node whose agent is not ready, so the
	// Pod reports ready only once its coordinator admits clients.
	if err := mgr.AddReadyzCheck("readyz", func(*http.Request) error {
		if !srv.Ready() {
			return errNotReady
		}
		return nil
	}); err != nil {
		return fmt.Errorf("add the readiness check: %w", err)
	}
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("run the manager: %w", err)
	}
	return nil
}

// newManager returns the manager the coordinator runs in. Every node's
// coordinator serves its own sessions, so there is no leader election, and the
// group resources live in their owners' namespaces, so the cache is
// cluster-wide. The cache reads only the kinds whose informers have started,
// the group kinds, which NewAttempts indexes and which the ServiceAccount may
// list. The manager's one port is the health endpoint's.
func newManager(cfg Config) (manager.Manager, error) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register the API's kinds: %w", err)
	}
	mgr, err := manager.New(cfg.RESTConfig, manager.Options{
		Scheme:                 scheme,
		Logger:                 cfg.Log,
		Cache:                  cache.Options{ReaderFailOnMissingInformer: true},
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.HealthPort)),
		// controller-runtime refuses a controller name already used in the
		// process, so that two controllers never report the same metrics. A
		// coordinator's process runs one manager, which serves no metrics, and
		// tests run Run more than once in a process.
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		return nil, fmt.Errorf("create the manager: %w", err)
	}
	return mgr, nil
}

// newAuthorizer returns the Authorizer of the agents' tokens, which reads where
// the members of an attempt run from attempts.
func newAuthorizer(ctx context.Context, cfg Config, attempts *groups.Attempts) (*agentauth.Authorizer, error) {
	clientset, err := kubernetes.NewForConfig(cfg.RESTConfig)
	if err != nil {
		return nil, fmt.Errorf("create the TokenReview client: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, issuerTimeout)
	defer cancel()
	auth, err := agentauth.New(ctx, agentauth.Config{
		Placement:      attempts,
		TokenReviews:   clientset.AuthenticationV1().TokenReviews(),
		RESTConfig:     cfg.RESTConfig,
		Audience:       cfg.Audience,
		ServiceAccount: cfg.ServiceAccount,
		Log:            cfg.Log.WithName("authorizer"),
	})
	if err != nil {
		return nil, fmt.Errorf("create the agents' Authorizer: %w", err)
	}
	return auth, nil
}

// shutdown shuts srv down within shutdownTimeout.
func shutdown(srv *server.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("shut the coordinator down: %w", err)
	}
	return nil
}

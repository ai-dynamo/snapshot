// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/coordinator/agentauth"
	"github.com/ai-dynamo/snapshot/coordinator/groups"
	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/config"
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

	// certPoll is how often Run checks for the serving certificate's files
	// while they are missing.
	certPoll = 2 * time.Second

	// storeDirName names the store directory, the server's Config.StoreDir,
	// in the temporary directory. A fixed name lets the next start empty a
	// store that a crashed coordinator left behind.
	storeDirName = "snapshot-coordinator"
)

// Run runs the coordinator that cfg configures until ctx is done, then shuts it
// down within a bounded time, failing every open session with
// session.ReasonCoordinatorLost. It builds the parts in the order package groups
// gives, and returns an error, having shut down what it started, if any of them
// fails. It serves the health endpoint first, before anything that can wait,
// such as the read of the API server's ServiceAccount issuer: the endpoint
// reports the process live from its start, and ready once the coordinator
// admits clients. Before server.New, it waits for the serving certificate's
// files if they are missing. It returns nil once ctx is done, also while it
// waits for those files, unless the manager, the health endpoint, or the
// shutdown fails.
func Run(ctx context.Context, cfg Config) (err error) {
	if err := cfg.validate(); err != nil {
		return err
	}
	health, err := serveHealth(net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.HealthPort)))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, health.stop()) }()
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
	certs, err := waitForCertificate(ctx, cfg.TLSDir, certPoll, cfg.Log)
	if err != nil {
		if ctx.Err() != nil {
			// ctx ended while Run waited.
			return nil
		}
		return err
	}
	if err := mgr.Add(certs); err != nil {
		return fmt.Errorf("watch the serving certificate: %w", err)
	}
	srv, err := server.New(server.Config{
		Host:            cfg.Host,
		Port:            cfg.Port,
		TLS:             &tls.Config{GetCertificate: certs.GetCertificate, MinVersion: tls.VersionTLS13},
		Authorizer:      auth,
		MaxBytes:        cfg.MaxBytes,
		MaxValue:        cfg.MaxValue,
		MaxLibraryValue: cfg.MaxLibraryValue,
		MaxLibraryKeys:  cfg.MaxLibraryKeys,
		StoreDir:        filepath.Join(os.TempDir(), storeDirName),
		Log:             cfg.Log.WithName("coordinator"),
	})
	if err != nil {
		return fmt.Errorf("start the coordinator: %w", err)
	}
	defer func() { err = errors.Join(err, shutdown(srv)) }()
	health.report(srv)
	if err := groups.Setup(mgr, attempts, groups.Config{Server: srv, Node: cfg.Node}); err != nil {
		return err
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
// list. The manager opens no port: Run serves the health endpoint itself, from
// before the manager starts.
func newManager(cfg Config) (manager.Manager, error) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register the API's kinds: %w", err)
	}
	mgr, err := manager.New(cfg.RESTConfig, manager.Options{
		Scheme:  scheme,
		Logger:  cfg.Log,
		Cache:   cache.Options{ReaderFailOnMissingInformer: true},
		Metrics: metricsserver.Options{BindAddress: "0"},
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

// waitForCertificate returns a watcher of the serving certificate in dir, once
// its files, tls.crt and tls.key, both exist. It checks for them every
// interval until they do or ctx is done, and logs once that it waits and once
// that they appeared. Files that exist but do not hold a certificate and its
// key are an error.
func waitForCertificate(ctx context.Context, dir string, interval time.Duration,
	log logr.Logger) (*certwatcher.CertWatcher, error) {
	certFile := filepath.Join(dir, corev1.TLSCertKey)
	keyFile := filepath.Join(dir, corev1.TLSPrivateKeyKey)
	waited := false
	if err := wait.PollUntilContextCancel(ctx, interval, true, func(context.Context) (bool, error) {
		found, err := allExist(certFile, keyFile)
		if !found && err == nil && !waited {
			log.Info("Waiting for the serving certificate", "directory", dir)
			waited = true
		}
		return found, err
	}); err != nil {
		return nil, fmt.Errorf("wait for the serving certificate: %w", err)
	}
	if waited {
		log.Info("Serving certificate found", "directory", dir)
	}
	certs, err := certwatcher.New(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("read the serving certificate: %w", err)
	}
	return certs, nil
}

// allExist reports whether every one of paths exists.
func allExist(paths ...string) (bool, error) {
	for _, path := range paths {
		_, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
	return true, nil
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

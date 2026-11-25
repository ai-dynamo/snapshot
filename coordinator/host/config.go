// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"fmt"
	"net"

	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
)

// Config configures Run. The limits are optional and default as server.Config
// says; every other field is required.
type Config struct {
	// RESTConfig reaches the API server, as the agents' ServiceAccount.
	RESTConfig *rest.Config

	// Node is the name of the node the coordinator runs on. It serves the
	// attempts whose rank-0 member runs there.
	Node string

	// ServiceAccount is the agents' ServiceAccount, whose Pod-bound tokens
	// the coordinator admits.
	ServiceAccount types.NamespacedName

	// Audience is the audience the agents' tokens are issued for.
	Audience string

	// Host is the IP address the coordinator listens on, the node's: neither
	// 0.0.0.0 nor ::, so it never listens on every interface. Port is the
	// client listener's port, and HealthPort the health endpoint's.
	Host             string
	Port, HealthPort int

	// TLSDir is the directory that holds the client listener's serving
	// certificate, tls.crt, and its key, tls.key.
	TLSDir string

	// MaxBytes, MaxValue, MaxLibraryValue and MaxLibraryKeys are the session
	// limits of server.Config.
	MaxBytes                  int64
	MaxValue, MaxLibraryValue int
	MaxLibraryKeys            int

	// Log receives what the coordinator logs, its manager included. It is
	// never given a key, a value, a barrier name, or a token.
	Log logr.Logger
}

// validate reports a missing or malformed field of cfg. server.New checks the
// limits.
func (cfg Config) validate() error {
	switch {
	case cfg.RESTConfig == nil:
		return fmt.Errorf("a coordinator needs the API server's REST config: %w", session.ErrInvalid)
	case cfg.Node == "":
		return fmt.Errorf("a coordinator needs the name of its node: %w", session.ErrInvalid)
	case cfg.ServiceAccount.Namespace == "" || cfg.ServiceAccount.Name == "":
		return fmt.Errorf("a coordinator needs the agents' ServiceAccount: %w", session.ErrInvalid)
	case cfg.Audience == "":
		return fmt.Errorf("a coordinator needs the audience of the agents' tokens: %w", session.ErrInvalid)
	case cfg.TLSDir == "":
		return fmt.Errorf("a coordinator needs the directory of its serving certificate: %w", session.ErrInvalid)
	}
	if ip := net.ParseIP(cfg.Host); ip == nil || ip.IsUnspecified() {
		return fmt.Errorf("a coordinator listens on one IP address, not %q: %w", cfg.Host, session.ErrInvalid)
	}
	for _, port := range []int{cfg.Port, cfg.HealthPort} {
		if port < 1 || port > 65535 {
			return fmt.Errorf("%d is not a port: %w", port, session.ErrInvalid)
		}
	}
	return nil
}

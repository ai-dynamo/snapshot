// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"net"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/ai-dynamo/snapshot/operator/internal/coordinatortls"
)

// bindCoordinatorTLSFlags binds the flags of the built-in issuer of the
// coordinator's TLS certificate. The chart sets all of them for the operator
// to issue the certificate, and none when the cluster provides it.
func bindCoordinatorTLSFlags(flags *flag.FlagSet) *coordinatortls.Config {
	cfg := &coordinatortls.Config{}
	flags.StringVar(&cfg.Namespace, "coordinator-tls-namespace", "",
		"Namespace of the coordinator's CA Secret, TLS Secret and CA bundle ConfigMap, which the operator issues")
	flags.StringVar(&cfg.CASecret, "coordinator-ca-secret", "",
		"Name of the Secret that holds the coordinator's CA certificate and key, which only the operator reads")
	flags.StringVar(&cfg.TLSSecret, "coordinator-tls-secret", "",
		"Name of the kubernetes.io/tls Secret that holds the coordinator's serving certificate and key")
	flags.StringVar(&cfg.CABundle, "coordinator-ca-bundle", "",
		"Name of the ConfigMap that holds the CA certificates the agents trust for the coordinator")
	flags.StringVar(&cfg.ServerName, "coordinator-server-name", "",
		"DNS name of the coordinator's serving certificate, which the agents verify")
	return cfg
}

// addCoordinatorTLSIssuer adds the issuer to mgr when its flags are set. With
// none set it adds nothing, so the operator writes none of the resources, and
// with only some set it returns an error.
func addCoordinatorTLSIssuer(mgr manager.Manager, cfg *coordinatortls.Config) error {
	flags := []struct{ name, value string }{
		{"--coordinator-tls-namespace", cfg.Namespace},
		{"--coordinator-ca-secret", cfg.CASecret},
		{"--coordinator-tls-secret", cfg.TLSSecret},
		{"--coordinator-ca-bundle", cfg.CABundle},
		{"--coordinator-server-name", cfg.ServerName},
	}
	var missing []string
	for _, f := range flags {
		if f.value == "" {
			missing = append(missing, f.name)
		}
	}
	switch len(missing) {
	case len(flags):
		return nil
	case 0:
	default:
		return fmt.Errorf("set every coordinator TLS flag or none: missing %s", strings.Join(missing, ", "))
	}
	// The CA Secret is Opaque and the serving Secret kubernetes.io/tls, and a
	// Secret's type cannot change. The ConfigMap may share either name.
	if cfg.CASecret == cfg.TLSSecret {
		return fmt.Errorf("--coordinator-ca-secret and --coordinator-tls-secret both name %q", cfg.CASecret)
	}
	// The certificate carries the server name as a DNS name only, and an IP
	// address verifies against IP addresses only. A name that never verifies
	// would make the issuer replace the serving certificate on every resync.
	if net.ParseIP(cfg.ServerName) != nil {
		return fmt.Errorf("--coordinator-server-name %q is an IP address, not a DNS name", cfg.ServerName)
	}
	if errs := validation.IsDNS1123Subdomain(cfg.ServerName); len(errs) > 0 {
		return fmt.Errorf("--coordinator-server-name %q: %s", cfg.ServerName, strings.Join(errs, "; "))
	}
	cfg.Clock = clock.RealClock{}
	cfg.Log = ctrl.Log.WithName("coordinator-tls")
	return mgr.Add(coordinatortls.NewIssuer(*cfg, mgr.GetAPIReader(), mgr.GetClient()))
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command snapshot-coordinator runs the Snapshot coordinator in a container of
// the agent's Pod; package host says what it needs.
package main

import (
	"flag"
	"os"

	"github.com/ai-dynamo/snapshot/coordinator/host"
	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// The default ports, on the node's network: the agents' is not NATS's 4222,
// and the health endpoint's is not the operator's 8080 or 8081.
const (
	defaultPort       = 4224
	defaultHealthPort = 4225
)

func main() {
	cfg, logOpts := bindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(newLogger(logOpts))

	restConfig, err := ctrl.GetConfig()
	if err != nil {
		ctrl.Log.Error(err, "unable to load the API server's configuration")
		os.Exit(1)
	}
	cfg.RESTConfig = restConfig
	cfg.Log = ctrl.Log
	if err := host.Run(ctrl.SetupSignalHandler(), *cfg); err != nil {
		ctrl.Log.Error(err, "the coordinator failed")
		os.Exit(1)
	}
}

// newLogger returns the logger that the logging options configure.
func newLogger(opts *zap.Options) logr.Logger { return zap.New(zap.UseFlagOptions(opts)) }

// bindFlags defines the coordinator's flags on fs, controller-runtime's zap
// flags included, and returns the Config and the logging options they set once
// fs has parsed them. The API server's configuration comes from
// controller-runtime. The logging options default to zap's production mode,
// which logs a malformed logging call instead of panicking, and logs no V(1)
// line.
func bindFlags(fs *flag.FlagSet) (*host.Config, *zap.Options) {
	cfg := &host.Config{}
	fs.StringVar(&cfg.Node, "node-name", "",
		"The name of the node the coordinator runs on, the agent Pod's spec.nodeName. Required.")
	fs.StringVar(&cfg.ServiceAccount.Namespace, "agent-namespace", "",
		"The namespace of the agents' ServiceAccount, the agent Pod's namespace. Required.")
	fs.StringVar(&cfg.ServiceAccount.Name, "agent-service-account", "",
		"The agents' ServiceAccount, the agent Pod's spec.serviceAccountName. Required.")
	fs.StringVar(&cfg.Audience, "token-audience", "",
		"The audience of the agents' ServiceAccount tokens. Required.")
	fs.StringVar(&cfg.Host, "listen-address", "",
		"The IP address to listen on for agents and for health probes, the node's: not 0.0.0.0 or ::. Required.")
	fs.IntVar(&cfg.Port, "port", defaultPort, "The port agents connect to, over TLS.")
	fs.IntVar(&cfg.HealthPort, "health-port", defaultHealthPort,
		"The port of the health endpoint, which serves /healthz and /readyz.")
	fs.StringVar(&cfg.TLSDir, "tls-cert-dir", "",
		"The directory that holds the serving certificate, tls.crt, and its key, tls.key, "+
			"which are served to new connections when they change. Required.")
	fs.Int64Var(&cfg.MaxBytes, "max-memory-bytes", server.DefaultMaxBytes,
		"The memory every session's data shares; about half the container's memory limit.")
	fs.IntVar(&cfg.MaxValue, "max-value-bytes", session.MaxValue,
		"The largest value an agent may write to a session.")
	fs.IntVar(&cfg.MaxLibraryValue, "max-library-value-bytes", session.MaxLibraryValue,
		"The largest value a communication library may write to a session.")
	fs.IntVar(&cfg.MaxLibraryKeys, "max-library-keys", session.MaxLibraryKeys,
		"How many keys communication libraries may write to each session, deleted ones included.")
	logOpts := &zap.Options{}
	logOpts.BindFlags(fs)
	return cfg, logOpts
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package main provides the snapshot-agent DaemonSet entrypoint.
// The agent runs the node-local snapshot controller and delegates CRIU/CUDA
// execution to the snapshot executor workflows.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/sync/errgroup"

	"github.com/ai-dynamo/snapshot/agent/internal/controller"
	"github.com/ai-dynamo/snapshot/agent/internal/logging"
	"github.com/ai-dynamo/snapshot/agent/internal/nri"
	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
)

func main() {
	runtimeType := flag.String("runtime", cmp.Or(os.Getenv("RUNTIME_TYPE"), snapshotruntime.RuntimeContainerd),
		"Container runtime backend: containerd or crio")
	runtimeSocket := flag.String("runtime-socket", os.Getenv("RUNTIME_SOCKET"),
		"Path to the container runtime socket (defaults to per-runtime convention)")
	flag.Parse()

	rootLog := logging.ConfigureLogger("stdout")
	agentLog := rootLog.WithName("agent")

	cfg, err := LoadConfigOrDefault(ConfigMapPath)
	if err != nil {
		fatal(agentLog, err, "Failed to load configuration")
	}
	if err := cfg.Validate(); err != nil {
		fatal(agentLog, err, "Invalid configuration")
	}
	// A host value that cannot be read is unknown, never fatal: the node keeps
	// capturing and restoring, and the checks that need it do not apply.
	if kernelVersion, err := snapshotruntime.ReadKernelVersion(snapshotruntime.HostProcPath); err != nil {
		agentLog.Error(err, "Failed to read the host kernel version; checkpoints taken here will not record it")
	} else {
		cfg.HostKernelVersion = kernelVersion
	}
	if cpuFeatures, err := snapshotruntime.ReadCPUFeatures(snapshotruntime.HostProcPath); err != nil {
		agentLog.Error(err, "Failed to read the host CPU features; checkpoints taken here will not record them")
	} else {
		cfg.HostCPUFeatures = cpuFeatures
	}

	rt, err := snapshotruntime.New(*runtimeType, *runtimeSocket)
	if err != nil {
		fatal(agentLog, err, "Failed to initialize container runtime",
			"runtime", *runtimeType, "socket", *runtimeSocket)
	}
	defer func() {
		if closeErr := rt.Close(); closeErr != nil {
			agentLog.Error(closeErr, "Failed to close runtime client")
		}
	}()

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(rootCtx, pageBrokerStartupTimeout)
	capabilities, err := waitForPageBroker(ctx, pagebroker.Client{ControlSocketPath: cfg.PageBroker.ControlSocketPath})
	cancel()
	if err != nil {
		fatal(agentLog, err, "Failed to read PageBroker capabilities")
	}
	cfg.CustomStorageAvailable = capabilities.CustomStorageAvailable

	agentLog.Info("Starting snapshot agent",
		"node", cfg.NodeName,
		"runtime", *runtimeType,
	)

	// The node controller handles both restore and capture paths.
	nodeController, err := controller.NewNodeController(cfg, rt, rootLog.WithName("controller"))
	if err != nil {
		fatal(agentLog, err, "Failed to create snapshot node controller")
	}
	// The controller, and the plugin when it registers, run until shutdown. If
	// either stops early, the agent exits and the DaemonSet restarts it.
	group, groupCtx := errgroup.WithContext(rootCtx)
	// The plugin registers before the controller runs, so every restore knows
	// whether the node has the restore proxy.
	plugin := nri.NewPlugin(nodeController, rootLog.WithName("nri"), os.Getenv(nri.HostNamespaceEnv))
	if err := plugin.Start(groupCtx); err != nil {
		agentLog.Error(err, "Restoring without the restore proxy: restore containers keep their own command as PID 1")
		nodeController.RestoreWithoutProxy()
	} else {
		group.Go(func() error {
			return untilShutdown(rootCtx, "NRI plugin", plugin.Wait(groupCtx))
		})
	}
	group.Go(func() error {
		return untilShutdown(rootCtx, "snapshot node controller", nodeController.Run(groupCtx))
	})
	if runErr := group.Wait(); runErr != nil {
		fatal(agentLog, runErr, "Snapshot agent exited with error")
	}

	agentLog.Info("Agent stopped")
}

// The sidecar allocates its GPU transfer rings before it creates the control socket.
const pageBrokerStartupTimeout = 5 * time.Minute

// waitForPageBroker retries while the sidecar is not yet listening.
func waitForPageBroker(ctx context.Context, client pagebroker.Client) (*pagebroker.Capabilities, error) {
	for {
		capabilities, err := client.Capabilities(ctx)
		if err == nil || !(errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)) {
			return capabilities, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(time.Second):
		}
	}
}

// untilShutdown turns an early, error-free return into an error, so the
// errgroup stops the other component instead of running on half an agent.
func untilShutdown(rootCtx context.Context, name string, err error) error {
	if err == nil && rootCtx.Err() == nil {
		return errors.New(name + " stopped unexpectedly")
	}
	return err
}

func fatal(log logr.Logger, err error, msg string, keysAndValues ...interface{}) {
	if err != nil {
		log.Error(err, msg, keysAndValues...)
	} else {
		log.Info(msg, keysAndValues...)
	}
	os.Exit(1)
}

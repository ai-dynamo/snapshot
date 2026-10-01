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

	"github.com/go-logr/logr"

	"github.com/ai-dynamo/snapshot/agent/internal/controller"
	"github.com/ai-dynamo/snapshot/agent/internal/cuda"
	"github.com/ai-dynamo/snapshot/agent/internal/logging"
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
	rootCtx, cancel := context.WithCancelCause(rootCtx)
	defer cancel(nil)
	var helper *cuda.Helper
	if cfg.CUDACheckpoint.Enabled {
		helper, err = cuda.StartHelper(rootCtx, cuda.DefaultHelperBinaryPath, cfg.CUDACheckpoint, agentLog)
		if err != nil {
			fatal(agentLog, err, "Failed to initialize CUDA helper")
		}
		defer helper.Stop()
		go func() {
			select {
			case <-helper.Done():
				cancel(helper.Err())
			case <-rootCtx.Done():
			}
		}()
	}

	agentLog.Info("Starting snapshot agent",
		"node", cfg.NodeName,
		"runtime", *runtimeType,
	)

	// The node controller handles both restore and capture paths.
	nodeController, err := controller.NewNodeController(cfg, rt, rootLog.WithName("controller"))
	if err != nil {
		helper.Stop()
		fatal(agentLog, err, "Failed to create snapshot node controller")
	}
	nodeController.SetCUDAHelper(helper)
	if runErr := nodeController.Run(rootCtx); runErr != nil {
		helper.Stop()
		fatal(agentLog, runErr, "Snapshot node controller exited with error")
	}

	cancel(nil)
	helper.Stop()
	if cause := context.Cause(rootCtx); cause != nil && !errors.Is(cause, context.Canceled) {
		fatal(agentLog, cause, "CUDA helper stopped unexpectedly")
	}
	agentLog.Info("Agent stopped")
}

func fatal(log logr.Logger, err error, msg string, keysAndValues ...interface{}) {
	if err != nil {
		log.Error(err, msg, keysAndValues...)
	} else {
		log.Info(msg, keysAndValues...)
	}
	os.Exit(1)
}

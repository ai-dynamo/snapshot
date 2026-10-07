// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// snapshot-restore-proxy runs as PID 1 in a restore destination container. It
// runs inside the user's image, so it is built static and must not touch file
// descriptors 0, 1 and 2: CRIU gives the restored process the stdio it finds
// on this process.
package main

import (
	"log/slog"
	"os"
	"os/signal"

	"github.com/go-logr/logr"

	"github.com/ai-dynamo/snapshot/agent/internal/restoreproxy"
)

func main() {
	// Subscribe first: PID 1 ignores any signal it has no handler for.
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, restoreproxy.ForwardedSignals...)

	os.Exit(restoreproxy.Run(restoreproxy.Config{
		ControlDir: restoreproxy.ControlDirFromEnv(),
		ProcRoot:   "/proc",
		Signals:    signals,
		// Not internal/logging: it links the container runtime clients, and
		// this binary is copied into every restore container.
		Log: logr.FromSlogHandler(slog.NewTextHandler(os.Stderr, nil)).WithName("snapshot-restore-proxy"),
	}))
}

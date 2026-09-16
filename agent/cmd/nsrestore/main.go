// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"syscall"

	"github.com/go-logr/logr"

	"github.com/ai-dynamo/snapshot/agent/internal/cuda"
	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	"github.com/ai-dynamo/snapshot/agent/internal/logging"
	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
)

func main() {
	// Logs go to stderr so stdout is reserved for the structured result.
	log := logging.ConfigureLogger("stderr").WithName("nsrestore")

	checkpointPath := flag.String("checkpoint-path", "", "Path to checkpoint directory")
	cudaDeviceMap := flag.String("cuda-device-map", "", "CUDA device map for cuda-checkpoint-helper restore")
	cgroupRoot := flag.String("cgroup-root", "", "CRIU cgroup root remap path")
	targetPodIP := flag.String("target-pod-ip", "", "Restore pod IP for CRIU TCP socket remapping")
	bundleDir := flag.String("bundle-dir", nsmount.SnapshotBinDst, "Path where the agent binary bundle is mounted in this namespace")
	allocationSessions := flag.String("allocation-sessions", "", "Inherited participant-to-bound-session FD map")
	flag.Parse()
	var sessions cuda.AllocationSessions
	if *allocationSessions != "" {
		var inherited map[string]int
		if err := json.Unmarshal([]byte(*allocationSessions), &inherited); err != nil {
			fatal(log, err, "invalid allocation session descriptor map")
		}
		sessions = make(cuda.AllocationSessions, len(inherited))
		seen := make(map[int]bool)
		for id, fd := range inherited {
			if fd < 10 || seen[fd] {
				fatal(log, nil, "invalid or duplicate allocation session descriptor")
			}
			seen[fd] = true
			// CRIU/helper children must never inherit the capability. The later
			// coordinator receives explicit ExtraFiles after native restore.
			syscall.CloseOnExec(fd)
			sessions[id] = os.NewFile(uintptr(fd), "allocation-session")
		}
		defer sessions.Close()
	}

	if *checkpointPath == "" {
		fatal(log, nil, "--checkpoint-path is required")
	}

	if err := useInjectedBundle(*bundleDir); err != nil {
		fatal(log, err, "failed to point lookups at the injected bundle")
	}

	opts := executor.RestoreOptions{
		AllocationSessions: sessions,
		CheckpointPath:     *checkpointPath,
		CUDADeviceMap:      *cudaDeviceMap,
		CgroupRoot:         *cgroupRoot,
		TargetPodIP:        *targetPodIP,
		BundleDir:          *bundleDir,
	}

	result, err := executor.RestoreInNamespace(context.Background(), opts, log)
	if err != nil {
		fatal(log, err, "restore failed")
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fatal(log, err, "Failed to write restore result")
	}
}

func fatal(log logr.Logger, err error, msg string) {
	if err != nil {
		log.Error(err, msg)
	} else {
		log.Info(msg)
	}
	os.Exit(1)
}

// useInjectedBundle points every binary and library lookup at the agent bundle
// mounted into this namespace. The placeholder ships no restore tooling, so
// criu, its shared libraries, and the binaries criu forks (ip, iptables-restore)
// must all resolve from the bundle.
//
// These are set on nsrestore's own environment rather than per-command: criu is
// launched by go-criu, and criu in turn forks ip/iptables-restore, so neither
// child is reachable through an exec.Cmd we control. Both inherit this environment.
// nsrestore itself is a static binary, so LD_LIBRARY_PATH does not affect it.
//
// The inherited PATH and LD_LIBRARY_PATH are read from the process environment
// directly — they arrive via the inherited env from the agent (execNSRestore sets
// cmd.Env = os.Environ()), so no flags are needed to pass them through argv.
func useInjectedBundle(bundleDir string) error {
	libDir := filepath.Join(bundleDir, "lib")
	if inherited := os.Getenv("LD_LIBRARY_PATH"); inherited != "" {
		libDir += ":" + inherited
	}
	if err := os.Setenv("LD_LIBRARY_PATH", libDir); err != nil {
		return err
	}
	newPATH := bundleDir
	if inherited := os.Getenv("PATH"); inherited != "" {
		newPATH = bundleDir + ":" + inherited
	}
	if err := os.Setenv("PATH", newPATH); err != nil {
		return err
	}
	return nil
}

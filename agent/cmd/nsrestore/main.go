// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	"github.com/go-logr/logr"
	"golang.org/x/sys/unix"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	"github.com/ai-dynamo/snapshot/agent/internal/logging"
	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
)

func main() {
	// Logs go to stderr so stdout is reserved for the structured result.
	log := logging.ConfigureLogger("stderr").WithName("nsrestore")

	checkpointPath := flag.String("checkpoint-path", "", "Path to checkpoint directory")
	var customStorage customStorageFlags
	flag.IntVar(&customStorage.pageBrokerSocketDirectoryFD, "pagebroker-socket-directory-fd", -1, "Inherited PageBroker socket directory")
	flag.IntVar(&customStorage.hostProcFD, "host-proc-fd", -1, "Inherited host proc directory for PID resolution")
	flag.IntVar(&customStorage.executionFD, "pagebroker-execution-fd", -1, "Inherited PageBroker execution socket")
	flag.IntVar(&customStorage.brokerProcessFD, "pagebroker-process-fd", -1, "Inherited PageBroker pidfd")
	flag.IntVar(&customStorage.cancelFD, "cancel-fd", -1, "Inherited cancellation pipe")
	flag.StringVar(&customStorage.pageBrokerSocketName, "pagebroker-socket-name", "", "PageBroker control socket name")
	flag.StringVar(&customStorage.gpuContext, "gpu-context", "", "GPU context JSON")
	flag.StringVar(&customStorage.pageBrokerTransactionID, "pagebroker-transaction", "", "PageBroker storage transaction")
	cudaDeviceMap := flag.String("cuda-device-map", "", "CUDA device map for cuda-checkpoint-helper restore")
	gpuMountAliases := flag.String("gpu-mount-aliases", "{}", "Checkpoint path to destination GPU path JSON")
	cgroupRoot := flag.String("cgroup-root", "", "CRIU cgroup root remap path")
	targetPodIP := flag.String("target-pod-ip", "", "Restore pod IP for CRIU TCP socket remapping")
	bundleDir := flag.String("bundle-dir", nsmount.SnapshotBinDst, "Path where the agent binary bundle is mounted in this namespace")
	flag.Parse()

	if *checkpointPath == "" {
		fatal(log, nil, "--checkpoint-path is required")
	}

	if err := useInjectedBundle(*bundleDir); err != nil {
		fatal(log, err, "failed to point lookups at the injected bundle")
	}

	opts := executor.RestoreOptions{
		CheckpointPath: *checkpointPath,
		CUDADeviceMap:  *cudaDeviceMap,
		CgroupRoot:     *cgroupRoot,
		TargetPodIP:    *targetPodIP,
		BundleDir:      *bundleDir,
	}

	var err error
	opts.CustomStorageExecution, opts.HostProc, err = parseCustomStorageOptions(flag.CommandLine, customStorage)
	if err != nil {
		fatal(log, err, "invalid CustomStorage options")
	}
	ctx := logr.NewContext(context.Background(), log)
	if opts.CustomStorageExecution != nil {
		defer opts.CustomStorageExecution.Close()
		defer opts.HostProc.Close()
		var cancel context.CancelFunc
		ctx, cancel = cancellationContext(ctx, os.NewFile(uintptr(customStorage.cancelFD), "restore-cancellation"))
		defer cancel()
	}

	if err := json.Unmarshal([]byte(*gpuMountAliases), &opts.GPUMountAliases); err != nil {
		fatal(log, err, "invalid GPU device paths")
	}
	result, err := executor.RestoreInNamespace(ctx, opts, log)
	if err != nil {
		fatal(log, err, "restore failed")
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fatal(log, err, "Failed to write restore result")
	}
}

func cancellationContext(ctx context.Context, file *os.File) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		var data [1]byte
		_, _ = file.Read(data[:])
		cancel()
	}()
	return ctx, func() {
		file.Close()
		cancel()
	}
}

const minimumInheritedDescriptor = 3

type customStorageFlags struct {
	pageBrokerSocketDirectoryFD int
	hostProcFD                  int
	executionFD                 int
	cancelFD                    int
	brokerProcessFD             int
	pageBrokerSocketName        string
	gpuContext                  string
	pageBrokerTransactionID     string
}

type inheritedDescriptor struct {
	name string
	fd   int
	kind uint32
}

func validateInheritedDescriptors(descriptors []inheritedDescriptor) error {
	for _, descriptor := range descriptors {
		var info unix.Stat_t
		if err := unix.Fstat(descriptor.fd, &info); err != nil {
			return fmt.Errorf("stat %s descriptor: %w", descriptor.name, err)
		}
		if info.Mode&unix.S_IFMT != descriptor.kind {
			return fmt.Errorf("invalid %s descriptor type", descriptor.name)
		}
		unix.CloseOnExec(descriptor.fd)
	}
	return nil
}

func parseCustomStorageOptions(flags *flag.FlagSet, input customStorageFlags) (*pagebroker.CustomStorageExecution, *os.File, error) {
	supplied := false
	flags.Visit(func(option *flag.Flag) {
		switch option.Name {
		case "pagebroker-process-fd", "pagebroker-socket-directory-fd", "host-proc-fd", "pagebroker-execution-fd", "cancel-fd", "pagebroker-socket-name", "gpu-context", "pagebroker-transaction":
			supplied = true
		}
	})
	if !supplied {
		return nil, nil, nil
	}
	if input.pageBrokerSocketDirectoryFD < minimumInheritedDescriptor || input.hostProcFD < minimumInheritedDescriptor ||
		input.executionFD < minimumInheritedDescriptor || input.cancelFD < minimumInheritedDescriptor || input.brokerProcessFD < minimumInheritedDescriptor ||
		input.pageBrokerSocketName == "" || input.pageBrokerTransactionID == "" || input.gpuContext == "" {
		return nil, nil, fmt.Errorf("CustomStorage requires socket and host proc directories, an execution socket, a cancellation pipe, a broker pidfd, a socket name, a transaction ID, and GPU context")
	}
	if err := validateInheritedDescriptors([]inheritedDescriptor{
		{"PageBroker socket directory", input.pageBrokerSocketDirectoryFD, unix.S_IFDIR},
		{"host proc directory", input.hostProcFD, unix.S_IFDIR},
		{"PageBroker execution socket", input.executionFD, unix.S_IFSOCK},
		{"cancellation pipe", input.cancelFD, unix.S_IFIFO},
	}); err != nil {
		return nil, nil, err
	}
	// A pidfd remains valid after namespace entry and after the broker exits.
	if err := unix.PidfdSendSignal(input.brokerProcessFD, 0, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) && !errors.Is(err, unix.EPERM) {
		return nil, nil, fmt.Errorf("invalid PageBroker process descriptor: %w", err)
	}
	unix.CloseOnExec(input.brokerProcessFD)
	gpuContext := new(pagebroker.GpuContext)
	if err := json.Unmarshal([]byte(input.gpuContext), gpuContext); err != nil {
		return nil, nil, fmt.Errorf("decode GPU context: %w", err)
	}
	execution := &pagebroker.CustomStorageExecution{
		SocketDirectory: os.NewFile(uintptr(input.pageBrokerSocketDirectoryFD), "pagebroker-socket-directory"),
		Socket:          os.NewFile(uintptr(input.executionFD), "pagebroker-execution"),
		BrokerProcess:   os.NewFile(uintptr(input.brokerProcessFD), "pagebroker-process"),
		SocketName:      input.pageBrokerSocketName,
		GPUContext:      gpuContext,
		TransactionID:   input.pageBrokerTransactionID,
	}
	return execution, os.NewFile(uintptr(input.hostProcFD), "host-proc"), nil
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
	libDir := filepath.Join(bundleDir, nsmount.BundleLibDir)
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

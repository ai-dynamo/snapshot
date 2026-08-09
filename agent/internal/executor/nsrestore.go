// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"time"

	criurpc "github.com/checkpoint-restore/go-criu/v8/rpc"
	"github.com/go-logr/logr"

	"github.com/ai-dynamo/snapshot/agent/internal/criu"
	"github.com/ai-dynamo/snapshot/agent/internal/cuda"
	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

// RestoreOptions holds configuration for an in-namespace restore.
type RestoreOptions struct {
	CheckpointPath          string
	CRIUBinaryFD            int
	CUDAHelperFD            int
	CUDADeviceMap           string
	CgroupRoot              string
	TargetPodIP             string
	PageBrokerImageFD       int
	PageBrokerWorkFD        int
	PageBrokerProviderFD    int
	PageBrokerControlFD     int
	PageBrokerTransactionID string
}

type RestoreInNamespaceResult struct {
	RestoredPID             int           `json:"restoredPID"`
	NSRestoreSetupDuration  time.Duration `json:"nsrestoreSetupDuration"`
	CRIUCoreDuration        time.Duration `json:"criuCoreDuration"`
	HostMemoryReadyDuration time.Duration `json:"hostMemoryReadyDuration"`
	CUDARestoreDuration     time.Duration `json:"cudaRestoreDuration"`
	CRIURestoreDuration     time.Duration `json:"criuRestoreDuration"`
}

// RestoreInNamespace performs a full restore from inside the target container's namespaces.
func RestoreInNamespace(ctx context.Context, opts RestoreOptions, log logr.Logger) (*RestoreInNamespaceResult, error) {
	restoreStart := time.Now()
	log.Info("Starting nsrestore workflow",
		"checkpoint_path", opts.CheckpointPath,
		"has_cuda_map", opts.CUDADeviceMap != "",
		"cgroup_root", opts.CgroupRoot,
		"target_pod_ip_present", opts.TargetPodIP != "",
	)

	manifestReadStart := time.Now()
	m, err := types.ReadManifest(opts.CheckpointPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read manifest: %w", err)
	}
	manifestReadDuration := time.Since(manifestReadStart)
	log.V(1).Info("Loaded checkpoint manifest",
		"ext_mounts", len(m.CRIUDump.ExtMnt),
		"criu_log_level", m.CRIUDump.CRIU.LogLevel,
		"manage_cgroups_mode", m.CRIUDump.CRIU.ManageCgroupsMode,
		"checkpoint_has_cuda", !m.CUDA.IsEmpty(),
	)

	// Phase 1: Configure — build CRIU opts from manifest
	configureStart := time.Now()
	if err := criu.ConfigureInetRemap(m, opts.TargetPodIP, log); err != nil {
		return nil, err
	}
	criuOpts, err := criu.BuildRestoreOpts(m, opts.CheckpointPath, opts.CgroupRoot, log)
	if err != nil {
		return nil, err
	}
	configureDuration := time.Since(configureStart)

	// Phase 2: Execute — rootfs and CRIU restore.
	executeTimings, restoredPID, err := executeRestore(ctx, criuOpts, m, opts, log)
	if err != nil {
		return nil, err
	}

	result := &RestoreInNamespaceResult{
		RestoredPID:             restoredPID,
		NSRestoreSetupDuration:  manifestReadDuration + configureDuration + executeTimings.nsrestoreSetupDuration,
		CRIUCoreDuration:        executeTimings.criuCoreDuration,
		HostMemoryReadyDuration: executeTimings.hostMemoryReadyDuration,
		CUDARestoreDuration:     executeTimings.cudaRestoreDuration,
		CRIURestoreDuration:     executeTimings.criuRestoreDuration,
	}
	log.V(1).Info("nsrestore timing summary",
		"restored_pid", restoredPID,
		"nsrestore_setup_duration", result.NSRestoreSetupDuration,
		"criu_core_duration", result.CRIUCoreDuration,
		"host_memory_ready_duration", result.HostMemoryReadyDuration,
		"cuda_restore_duration", result.CUDARestoreDuration,
		"criu_restore_duration", result.CRIURestoreDuration,
		"total_duration", time.Since(restoreStart),
	)
	return result, nil
}

type nsrestorePhaseTimings struct {
	nsrestoreSetupDuration  time.Duration
	criuCoreDuration        time.Duration
	hostMemoryReadyDuration time.Duration
	cudaRestoreDuration     time.Duration
	criuRestoreDuration     time.Duration
}

func executeRestore(ctx context.Context, criuOpts *criurpc.CriuOpts, m *types.CheckpointManifest, opts RestoreOptions, log logr.Logger) (*nsrestorePhaseTimings, int, error) {
	timings := &nsrestorePhaseTimings{}
	// Apply rootfs diff inside the namespace (target root is /)
	nsrestoreSetupStart := time.Now()
	if err := snapshotruntime.ApplyRootfsDiff(opts.CheckpointPath, "/", log); err != nil {
		return nil, 0, fmt.Errorf("rootfs diff failed: %w", err)
	}

	if err := snapshotruntime.ApplyDeletedFiles(opts.CheckpointPath, "/", log); err != nil {
		log.Error(err, "Failed to apply deleted files")
	}

	// Unmount placeholder's /dev/shm so CRIU can recreate tmpfs with checkpointed content
	if err := syscall.Unmount("/dev/shm", 0); err != nil {
		return nil, 0, fmt.Errorf("failed to unmount /dev/shm before restore: %w", err)
	}

	if err := snapshotruntime.RemountProcSys(true); err != nil {
		return nil, 0, fmt.Errorf("failed to remount /proc/sys read-write for restore: %w", err)
	}
	timings.nsrestoreSetupDuration = time.Since(nsrestoreSetupStart)
	defer func() {
		if err := snapshotruntime.RemountProcSys(false); err != nil {
			log.Error(err, "Failed to remount /proc/sys read-only after restore")
		}
	}()

	var providerForCRIU *os.File
	if opts.PageBrokerProviderFD >= 0 {
		providerSocket := os.NewFile(uintptr(opts.PageBrokerProviderFD), "pagebroker-provider")
		if providerSocket == nil {
			return nil, 0, fmt.Errorf("open inherited PageBroker provider socket fd %d", opts.PageBrokerProviderFD)
		}
		defer providerSocket.Close()
		fd, err := syscall.Dup(int(providerSocket.Fd()))
		if err != nil {
			return nil, 0, fmt.Errorf("duplicate PageBroker provider socket: %w", err)
		}
		providerForCRIU = os.NewFile(uintptr(fd), "pagebroker-provider-criu")
		// CRIU owns this duplicate until its cleanup callback returns. The parent
		// endpoint remains open so the provider can serve CRIU throughout restore.
		defer providerForCRIU.Close()
	}
	var pageBrokerControl *os.File
	if opts.PageBrokerControlFD >= 0 {
		if opts.PageBrokerTransactionID == "" {
			return nil, 0, fmt.Errorf("PageBroker transaction ID is required with readiness control fd")
		}
		pageBrokerControl = os.NewFile(uintptr(opts.PageBrokerControlFD), "pagebroker-control")
		if pageBrokerControl == nil {
			return nil, 0, fmt.Errorf("open inherited PageBroker control fd %d", opts.PageBrokerControlFD)
		}
		defer pageBrokerControl.Close()
	}

	// CRIU restore
	criuRestoreStart := time.Now()
	preResume := restorePreResume(
		pageBrokerControl,
		opts.PageBrokerTransactionID,
		pagebroker.WaitReady,
		criuRestoreStart,
		timings,
		log,
	)
	restoredPID, cleanup, err := criu.ExecuteRestore(criuOpts, m, opts.CheckpointPath, opts.CRIUBinaryFD, opts.PageBrokerImageFD, opts.PageBrokerWorkFD, providerForCRIU, preResume, log)
	if err != nil {
		return nil, 0, err
	}
	// Run the restore FD cleanup when this function returns, including errors.
	defer cleanup()
	timings.criuRestoreDuration = time.Since(criuRestoreStart)

	// CRIU has resumed the workload. This is intentionally a best-effort
	// agent-owned CUDA restore: the CUDA restore thread must be runnable for
	// cuCheckpointProcessRestore to complete, so it cannot run in pre-resume
	// while CRIU still ptrace-stops every restored task.
	if !m.CUDA.IsEmpty() {
		if opts.CUDAHelperFD < 0 {
			return nil, 0, fmt.Errorf("agent CUDA helper FD is required for CUDA restore")
		}
		cudaStart := time.Now()
		processes, err := snapshotruntime.ReadProcessTable("/proc")
		if err != nil {
			return nil, 0, fmt.Errorf("read restored process table: %w", err)
		}
		restorePIDs, err := snapshotruntime.ResolveManifestPIDsToObservedPIDs(processes, int(restoredPID), m.CUDA.PIDs)
		if err != nil {
			return nil, 0, fmt.Errorf("resolve restored CUDA PIDs: %w", err)
		}
		log.Info("Restoring CUDA through the agent after CRIU resume", "restored_cuda_pids", restorePIDs)
		helperBinary := fmt.Sprintf("/proc/self/fd/%d", opts.CUDAHelperFD)
		if _, err := cuda.RestoreAndUnlockProcessTree(ctx, restorePIDs, opts.CUDADeviceMap, helperBinary, log); err != nil {
			return nil, 0, fmt.Errorf("agent CUDA restore: %w", err)
		}
		timings.cudaRestoreDuration = time.Since(cudaStart)
	}

	return timings, int(restoredPID), nil
}

func restorePreResume(
	control *os.File,
	transactionID string,
	waitReady func(*os.File, string) error,
	criuRestoreStart time.Time,
	timings *nsrestorePhaseTimings,
	log logr.Logger,
) func(int32) error {
	return func(restoredPID int32) error {
		timings.criuCoreDuration = time.Since(criuRestoreStart)
		if control != nil {
			waitStart := time.Now()
			if err := waitReady(control, transactionID); err != nil {
				return fmt.Errorf("PageBroker host memory readiness: %w", err)
			}
			timings.hostMemoryReadyDuration = time.Since(waitStart)
			log.Info("PageBroker host memory ready at CRIU pre-resume",
				"transaction_id", transactionID,
				"restored_pid", restoredPID,
				"wait_duration", timings.hostMemoryReadyDuration,
			)
		}
		return nil
	}
}

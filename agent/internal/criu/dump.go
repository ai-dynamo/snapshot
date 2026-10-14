// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	criulib "github.com/checkpoint-restore/go-criu/v8"
	criurpc "github.com/checkpoint-restore/go-criu/v8/rpc"
	"github.com/go-logr/logr"
	"google.golang.org/protobuf/proto"

	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

const (
	dumpLogFilename    = "dump.log"
	criuConfFilename   = "criu.conf"
	dumpLogTailMaxSize = 16 * 1024
)

// BuildDumpOptions creates CRIU options from the container snapshot and settings.
// It also writes the criu.conf file for options that cannot be passed via RPC.
// The ImagesDirFd is left unset — ExecuteDump opens it at dump time.
func BuildDumpOptions(
	state *types.CheckpointContainerSnapshot,
	settings *types.CRIUSettings,
	checkpointDir string,
	log logr.Logger,
) (*criurpc.CriuOpts, error) {
	var maskedPaths []string
	if state.OCISpec != nil && state.OCISpec.Linux != nil {
		maskedPaths = state.OCISpec.Linux.MaskedPaths
	}

	externalized, skipped := snapshotruntime.BuildMountPolicy(state.Mounts, state.RootFS, maskedPaths)
	log.V(1).Info("Resolved mount policy for CRIU dump",
		"externalized_count", len(externalized),
		"skipped_count", len(skipped),
	)

	criuOpts := &criurpc.CriuOpts{
		Pid:     proto.Int32(int32(state.PID)),
		Root:    proto.String(state.RootFS),
		LogFile: proto.String(dumpLogFilename),
		// The dump always terminates the source process. Leaving it running is
		// not a supported mode (a CUDA-locked source is never resumed), and the
		// operator treats the source's death as the expected outcome of a
		// successful capture.
		LeaveRunning: proto.Bool(false),
		// Always externalize network namespace
		External: []string{fmt.Sprintf("net[%d]:extNetNs", state.NetNSInode)},
	}
	criuOpts.ExtMnt = toExtMountMaps(externalized)
	criuOpts.SkipMnt = skipped

	if state.HostCgroupPath != "" {
		criuOpts.FreezeCgroup = proto.String(state.HostCgroupPath)
	}

	if settings == nil {
		return criuOpts, nil
	}

	if err := applyCommonSettings(criuOpts, settings); err != nil {
		return nil, err
	}

	// Dump-only options
	criuOpts.OrphanPtsMaster = proto.Bool(settings.OrphanPtsMaster)
	criuOpts.ExtMasters = proto.Bool(settings.ExtMasters)
	criuOpts.AutoDedup = proto.Bool(settings.AutoDedup)
	criuOpts.LazyPages = proto.Bool(settings.LazyPages)

	if settings.GhostLimit > 0 {
		criuOpts.GhostLimit = proto.Uint32(settings.GhostLimit)
	}

	// Write criu.conf for options that cannot be passed via RPC.
	if confContent := buildCRIUConf(settings); confContent != "" {
		confPath := filepath.Join(checkpointDir, criuConfFilename)
		if err := os.WriteFile(confPath, []byte(confContent), 0644); err != nil {
			return nil, fmt.Errorf("failed to write criu.conf: %w", err)
		}
		criuOpts.ConfigFile = proto.String(confPath)
	}

	return criuOpts, nil
}

// DumpExecution is the agent's view of one CRIU dump.
//
// Duration is the wall time of the dump RPC. ImageGrowth samples the image
// directory while that RPC runs, so a stall (size flat) is distinguishable
// from a steady write. CRIU's own memdump/memwrite timers are read afterwards
// from stats-dump; see DumpStats.
type DumpExecution struct {
	Duration    time.Duration
	ImageGrowth []ImageGrowthSample
}

// ExecuteDump opens the image directory FD, runs the CRIU dump, and cleans up.
func ExecuteDump(
	criuOpts *criurpc.CriuOpts,
	checkpointDir string,
	settings *types.CRIUSettings,
	log logr.Logger,
) (DumpExecution, error) {
	imageDir, imageDirFD, err := openPathForCRIU(checkpointDir)
	if err != nil {
		return DumpExecution{}, fmt.Errorf("failed to open image directory: %w", err)
	}
	defer imageDir.Close()
	criuOpts.ImagesDirFd = proto.Int32(imageDirFD)

	criuClient := criulib.MakeCriu()
	if settings != nil && strings.TrimSpace(settings.BinaryPath) != "" {
		if _, err := os.Stat(settings.BinaryPath); err != nil {
			return DumpExecution{}, fmt.Errorf("criu binary not found at %s: %w", settings.BinaryPath, err)
		}
		criuClient.SetCriuPath(settings.BinaryPath)
	}

	criuDumpStart := time.Now()
	stopGrowth := sampleImageGrowth(checkpointDir)
	defer func() {
		if stopGrowth != nil {
			stopGrowth()
		}
	}()
	dumpErr := criuClient.Dump(criuOpts, nil)
	// The sampler's last directory walk happens after the RPC. Reading the
	// clock first keeps that walk out of the reported dump duration.
	criuDumpDuration := time.Since(criuDumpStart)
	growth := stopGrowth()
	stopGrowth = nil
	if dumpErr != nil {
		dumpLogPath := filepath.Join(checkpointDir, dumpLogFilename)
		log.Error(dumpErr, "CRIU dump failed",
			"duration", criuDumpDuration,
			"checkpoint_dir", checkpointDir,
			"dump_log_path", dumpLogPath,
			"skip_mnt", criuOpts.GetSkipMnt(),
			"dump_log_tail", readLogTail(dumpLogPath),
		)
		return DumpExecution{}, fmt.Errorf("CRIU dump failed: %w", dumpErr)
	}

	log.Info("CRIU dump completed", "duration", criuDumpDuration, "growth_samples", len(growth))
	return DumpExecution{Duration: criuDumpDuration, ImageGrowth: growth}, nil
}

func readLogTail(path string) string {
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("failed to read dump log: %v", err)
	}
	if len(content) <= dumpLogTailMaxSize {
		return string(content)
	}
	return "...<truncated>...\n" + string(content[len(content)-dumpLogTailMaxSize:])
}

func buildCRIUConf(c *types.CRIUSettings) string {
	if c == nil {
		return ""
	}
	var content string
	if c.LibDir != "" {
		content += "libdir " + c.LibDir + "\n"
	}
	if c.AllowUprobes {
		content += "allow-uprobes\n"
	}
	if c.SkipInFlight {
		content += "skip-in-flight\n"
	}
	return content
}

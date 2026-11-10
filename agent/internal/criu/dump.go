// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	criulib "github.com/checkpoint-restore/go-criu/v8"
	criurpc "github.com/checkpoint-restore/go-criu/v8/rpc"
	"github.com/go-logr/logr"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"

	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

const (
	dumpLogFilename    = "dump.log"
	criuConfFilename   = "criu.conf"
	dumpLogTailMaxSize = 16 * 1024
)

// Version reports the CRIU that will run, formatted major.minor.sublevel.
//
// CRIU moves under the agent — the reference is pinned by image, not by this
// repository — so a dump failure is otherwise impossible to attribute to a
// version from a bug report alone.
func Version(settings *types.CRIUSettings) (string, error) {
	client := criulib.MakeCriu()
	if settings != nil && strings.TrimSpace(settings.BinaryPath) != "" {
		client.SetCriuPath(settings.BinaryPath)
	}
	return versionOf(client)
}

func versionOf(client *criulib.Criu) (string, error) {
	encoded, err := client.GetCriuVersion()
	if err != nil {
		return "", err
	}
	return formatCriuVersion(encoded), nil
}

// CRIU encodes its version as major*10000 + minor*100 + sublevel.
func formatCriuVersion(encoded int) string {
	return fmt.Sprintf("%d.%d.%d", encoded/10000, (encoded/100)%100, encoded%100)
}

// versionOrUnknown never fails the caller: a dump that already failed is not
// made worse by being unable to name the CRIU that failed it.
func versionOrUnknown(client *criulib.Criu) string {
	version, err := versionOf(client)
	if err != nil {
		return "unknown"
	}
	return version
}

// withFullDiskErrno re-reports a full staging volume as the errno for it.
//
// CRIU surfaces ENOSPC only as message text, so the string is matched here
// rather than by every caller that needs to tell a full volume from anything else.
func withFullDiskErrno(err error) error {
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), unix.ENOSPC.Error()) {
		return err
	}
	return errors.Join(err, unix.ENOSPC)
}

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

// ExecuteDump opens the image directory FD, runs the CRIU dump, and cleans up.
func ExecuteDump(
	criuOpts *criurpc.CriuOpts,
	checkpointDir string,
	settings *types.CRIUSettings,
	log logr.Logger,
) (time.Duration, error) {
	imageDir, imageDirFD, err := openPathForCRIU(checkpointDir)
	if err != nil {
		return 0, fmt.Errorf("failed to open image directory: %w", err)
	}
	defer imageDir.Close()
	criuOpts.ImagesDirFd = proto.Int32(imageDirFD)

	criuDumpStart := time.Now()
	criuClient := criulib.MakeCriu()
	if settings != nil && strings.TrimSpace(settings.BinaryPath) != "" {
		if _, err := os.Stat(settings.BinaryPath); err != nil {
			return 0, fmt.Errorf("criu binary not found at %s: %w", settings.BinaryPath, err)
		}
		criuClient.SetCriuPath(settings.BinaryPath)
	}
	if err := criuClient.Dump(criuOpts, nil); err != nil {
		dumpDuration := time.Since(criuDumpStart)
		dumpLogPath := filepath.Join(checkpointDir, dumpLogFilename)
		log.Error(err, "CRIU dump failed",
			"duration", dumpDuration,
			// Asked only here: the round trip costs nothing against a dump that
			// has already failed, and this is the line a bug report quotes.
			"criu_version", versionOrUnknown(criuClient),
			"checkpoint_dir", checkpointDir,
			"dump_log_path", dumpLogPath,
			"skip_mnt", criuOpts.GetSkipMnt(),
			"dump_log_tail", readLogTail(dumpLogPath),
		)
		return 0, withFullDiskErrno(fmt.Errorf("CRIU dump failed: %w", err))
	}

	criuDumpDuration := time.Since(criuDumpStart)
	log.Info("CRIU dump completed", "duration", criuDumpDuration)
	return criuDumpDuration, nil
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

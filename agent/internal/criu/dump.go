// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// versionTimeout bounds asking CRIU what it is. Overridden in tests.
var versionTimeout = 5 * time.Second

// versionWaitDelay bounds the wait for the version command's output after its
// deadline. Killing the process does not close pipes a grandchild inherited,
// and a configured wrapper can leave one holding them.
const versionWaitDelay = time.Second

// DefaultBinary is the CRIU the agent runs when its configuration names none.
const DefaultBinary = "criu"

// ResolveBinary names the CRIU the agent will run.
//
// One rule for every caller: LookPath takes a bare name from PATH and a path as
// given, and reports either being unusable the same way. Resolving differently
// per caller is how the version reported at startup comes to name a binary no
// dump can run.
func ResolveBinary(settings *types.CRIUSettings) (string, error) {
	name := DefaultBinary
	if settings != nil && strings.TrimSpace(settings.BinaryPath) != "" {
		name = strings.TrimSpace(settings.BinaryPath)
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("criu binary %q is not usable: %w", name, err)
	}
	return path, nil
}

// Version reports the CRIU that will run: the version it states, and for a
// build from git the commit it was built at.
//
// CRIU moves under the agent — the reference is pinned by image, not by this
// repository, and defaults to a development branch — so neither the version
// nor the commit identifies a build on its own.
//
// It asks the binary rather than the RPC. go-criu folds a git build's sublevel
// into the minor number ("if it is a git release -> increase minor by 1"), so
// the number it returns is not the one CRIU states, and it discards the commit
// the same response carries.
func Version(ctx context.Context, settings *types.CRIUSettings) (string, error) {
	binary, err := ResolveBinary(settings)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--version")
	// The deadline kills the process; without this it would not end a wait on
	// output pipes something else still holds open.
	cmd.WaitDelay = versionWaitDelay
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ask %s its version: %w", binary, err)
	}
	return parseVersion(string(out))
}

// Each field is on its own line, so the value cannot be read from the next one:
// \s* would cross the newline and take an empty Version: as whatever follows.
var (
	versionLine = regexp.MustCompile(`(?m)^Version:[ \t]*(\S+)`)
	gitIDLine   = regexp.MustCompile(`(?m)^GitID:[ \t]*(\S+)`)
)

// parseVersion reads what `criu --version` states. GitID is printed only for a
// build from git, so its absence is a release build rather than a failure.
func parseVersion(out string) (string, error) {
	version := versionLine.FindStringSubmatch(out)
	if version == nil {
		return "", fmt.Errorf("no version in %q", strings.TrimSpace(out))
	}
	if gitID := gitIDLine.FindStringSubmatch(out); gitID != nil {
		return fmt.Sprintf("%s (%s)", version[1], gitID[1]), nil
	}
	return version[1], nil
}

// versionOrUnknown never fails the caller: a dump that already failed is not
// made worse by being unable to name the CRIU that failed it.
func versionOrUnknown(ctx context.Context, settings *types.CRIUSettings) string {
	version, err := Version(ctx, settings)
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
// It returns the wall time of the dump RPC. CRIU's own memdump/memwrite timers
// are read afterwards from stats-dump; see DumpStats.
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

	criuClient := criulib.MakeCriu()
	binary, err := ResolveBinary(settings)
	if err != nil {
		return 0, err
	}
	criuClient.SetCriuPath(binary)

	criuDumpStart := time.Now()
	dumpErr := criuClient.Dump(criuOpts, nil)
	criuDumpDuration := time.Since(criuDumpStart)
	if dumpErr != nil {
		dumpLogPath := filepath.Join(checkpointDir, dumpLogFilename)
		log.Error(dumpErr, "CRIU dump failed",
			"duration", criuDumpDuration,
			// Asked only here: the round trip costs nothing against a dump that
			// has already failed, and this is the line a bug report quotes.
			"criu_version", versionOrUnknown(context.Background(), settings),
			"checkpoint_dir", checkpointDir,
			"dump_log_path", dumpLogPath,
			"skip_mnt", criuOpts.GetSkipMnt(),
			"dump_log_tail", readLogTail(dumpLogPath),
		)
		return 0, withFullDiskErrno(fmt.Errorf("CRIU dump failed: %w", dumpErr))
	}

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

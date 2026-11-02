// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecyclehooks

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	remote "k8s.io/cri-client/pkg"
)

// Execer runs a command in a container the way kubelet runs an exec probe:
// as the container's user, with its capabilities, seccomp and LSM profile,
// inside its mounts. runtime.Runtime implements it through CRI ExecSync.
//
// File actions go through it so that they do only what the application's
// own user could do. The agent never touches the files as root.
type Execer interface {
	ExecSync(ctx context.Context, containerID string, cmd []string, timeout time.Duration) (stdout, stderr []byte, err error)
}

// The scripts report what they found with these exit codes. They are above
// the codes shells use for their own errors (1 and 2, 126 and 127, and 128
// plus a signal), so a failure of the shell itself, such as a redirect that
// can't create the file, is never read as one of these answers.
const (
	exitMissing        = 10
	exitNotRegular     = 11
	exitExists         = 12
	exitNoParent       = 13
	exitParentReadOnly = 14
)

// The scripts take the path as $1, never as part of the script, so no path
// needs quoting. They follow symlinks: the command runs as the container, so
// a link can only lead where the application itself could go.
var (
	// fileExistsScript exits 0 for a regular file, exitMissing when nothing
	// is at the path, and exitNotRegular for anything else.
	fileExistsScript = fmt.Sprintf(`[ -f "$1" ] && exit 0; [ -e "$1" ] && exit %d; exit %d`,
		exitNotRegular, exitMissing)
	// existingScript is the start of the two marker scripts: it exits
	// exitExists when a regular file is at the path, and exitNotRegular for
	// anything else there.
	existingScript = fmt.Sprintf(`if [ -e "$1" ]; then [ -f "$1" ] && exit %d; exit %d; fi; `,
		exitExists, exitNotRegular)
	// createMarkerScript creates an empty file, never its parents. set -C
	// (noclobber) makes the shell create it with O_EXCL, so a file that
	// appears in between is not overwritten.
	createMarkerScript = existingScript + `set -C; : > "$1"`
	// checkMarkerTargetScript changes nothing. It exits exitNoParent when the
	// parent is not a directory, and exitParentReadOnly when the user may not
	// write it: [ -w ] asks the kernel as the user, so a read-only mount
	// counts too.
	checkMarkerTargetScript = existingScript + fmt.Sprintf(
		`d="${1%%/*}"; [ -n "$d" ] || d=/; [ -d "$d" ] || exit %d; [ -w "$d" ] || exit %d`,
		exitNoParent, exitParentReadOnly)
)

const (
	// maxOutput bounds the command output kept for an error, as kubelet bounds
	// an exec probe's.
	maxOutput = 10 << 10
)

// FileExists reports whether a regular file exists at filePath in the
// container. A missing file is false. Anything else at the path is an error,
// so a caller never reads it as either answer.
func FileExists(ctx context.Context, e Execer, containerID, filePath string, timeout time.Duration) (bool, error) {
	code, output, err := runScript(ctx, e, containerID, fileExistsScript, filePath, timeout)
	if err != nil {
		return false, err
	}
	switch code {
	case 0:
		return true, nil
	case exitMissing:
		return false, nil
	case exitNotRegular:
		return false, fmt.Errorf("%s in container %s is not a regular file", filePath, containerID)
	default:
		return false, fmt.Errorf("failed to check %s in container %s: exit code %d: %s", filePath, containerID, code, output)
	}
}

// CreateMarker creates an empty regular file at filePath in the container, as
// the container's user. The parent directory must exist. A regular file that
// is already there is left alone, and the error matches fs.ErrExist; the
// caller decides whether that file is its own.
func CreateMarker(ctx context.Context, e Execer, containerID, filePath string, timeout time.Duration) error {
	code, output, err := runScript(ctx, e, containerID, createMarkerScript, filePath, timeout)
	if err != nil {
		return err
	}
	switch code {
	case 0:
		return nil
	case exitExists:
		return fmt.Errorf("%s in container %s: %w", filePath, containerID, fs.ErrExist)
	case exitNotRegular:
		return fmt.Errorf("%s in container %s exists and is not a regular file", filePath, containerID)
	default:
		return fmt.Errorf("failed to create %s in container %s: %s", filePath, containerID, output)
	}
}

// CheckMarkerTarget reports whether CreateMarker could create filePath in the
// container now, without creating it: nothing is at the path, and its parent
// is a directory the container's user may write. A regular file already there
// matches fs.ErrExist, as with CreateMarker.
//
// A restore runs it before CRIU, so a path the marker can't be written to,
// such as one on a read-only mount, fails the restore before the work is done
// rather than after.
func CheckMarkerTarget(ctx context.Context, e Execer, containerID, filePath string, timeout time.Duration) error {
	code, output, err := runScript(ctx, e, containerID, checkMarkerTargetScript, filePath, timeout)
	if err != nil {
		return err
	}
	switch code {
	case 0:
		return nil
	case exitExists:
		return fmt.Errorf("%s in container %s: %w", filePath, containerID, fs.ErrExist)
	case exitNotRegular:
		return fmt.Errorf("%s in container %s exists and is not a regular file", filePath, containerID)
	case exitNoParent:
		return fmt.Errorf("the directory of %s does not exist in container %s", filePath, containerID)
	case exitParentReadOnly:
		return fmt.Errorf("the container's user can't write the directory of %s in container %s", filePath, containerID)
	default:
		return fmt.Errorf("failed to check %s in container %s: exit code %d: %s", filePath, containerID, code, output)
	}
}

// runScript runs script with /bin/sh in the container and returns its exit
// code and error output. The error is set only when the script didn't run to
// an exit code: the runtime failed, or the command timed out.
func runScript(ctx context.Context, e Execer, containerID, script, filePath string, timeout time.Duration) (int, string, error) {
	cmd := []string{"/bin/sh", "-c", script, "sh", filePath}
	_, stderr, err := e.ExecSync(ctx, containerID, cmd, timeout)
	if err == nil {
		return 0, "", nil
	}
	var exit interface{ ExitStatus() int }
	if errors.As(err, &exit) {
		return exit.ExitStatus(), output(stderr), nil
	}
	if errors.Is(err, remote.ErrCommandTimedOut) {
		return 0, "", fmt.Errorf("failed to run the file action on %s in container %s: %w", filePath, containerID, err)
	}
	return 0, "", fmt.Errorf("failed to run /bin/sh in container %s; file actions need a shell in the image: %w", containerID, err)
}

func output(b []byte) string {
	if len(b) > maxOutput {
		b = b[:maxOutput]
	}
	return strings.TrimSpace(string(b))
}

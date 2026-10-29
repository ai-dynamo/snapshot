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

// The scripts take the path as $1, never as part of the script, so no path
// needs quoting. They follow symlinks: the command runs as the container, so
// a link can only lead where the application itself could go.
const (
	// fileExistsScript exits 0 for a regular file, exitMissing when nothing
	// is at the path, and exitNotRegular for anything else.
	fileExistsScript = `[ -f "$1" ] && exit 0; [ -e "$1" ] && exit 2; exit 1`
	// createMarkerScript creates an empty file, never its parents. set -C
	// (noclobber) makes the shell create it with O_EXCL, so a file that
	// appears in between is not overwritten. It exits exitExists when a
	// regular file is already there, and exitNotRegular for anything else.
	createMarkerScript = `if [ -e "$1" ]; then [ -f "$1" ] && exit 3; exit 2; fi; set -C; : > "$1"`

	exitMissing    = 1
	exitNotRegular = 2
	exitExists     = 3

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

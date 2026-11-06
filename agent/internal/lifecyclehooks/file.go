// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecyclehooks

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
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
	exitMissing    = 10
	exitNotRegular = 11
	exitExists     = 12
)

// The scripts take the path as $1, never as part of the script, so no path
// needs quoting. They follow symlinks: the command runs as the container, so
// a link can only lead where the application itself could go.
var (
	// fileExistsScript exits 0 for a regular file, exitMissing when nothing
	// is at the path, and exitNotRegular for anything else.
	fileExistsScript = fmt.Sprintf(`[ -f "$1" ] && exit 0; [ -e "$1" ] && exit %d; exit %d`,
		exitNotRegular, exitMissing)
	// existingScript is the start of the marker script: it exits
	// exitExists when a regular file is at the path, and exitNotRegular for
	// anything else there, a dangling symlink included: [ -e ] doesn't see
	// one, but the create would fail on it.
	existingScript = fmt.Sprintf(`if [ -e "$1" ] || [ -L "$1" ]; then [ -f "$1" ] && exit %d; exit %d; fi; `,
		exitExists, exitNotRegular)
	// createMarkerScript creates an empty file, never its parents. set -C
	// (noclobber) makes the shell create it with O_EXCL, so a file that
	// appears in between is not overwritten.
	createMarkerScript = existingScript + `set -C; : > "$1"`
)

const (
	// maxOutput bounds the command output kept for an error, as kubelet bounds
	// an exec probe's.
	maxOutput = 10 << 10
)

// shells are the commands that can run the scripts, tried in order until one
// runs in the container. Most images have /bin/sh. BusyBox has it as an applet, and the
// distroless debug images keep BusyBox under /busybox. The scripts use only
// POSIX shell built-ins, so any of these runs them the same way.
var shells = [][]string{
	{"/bin/sh", "-c"},
	{"/usr/bin/sh", "-c"},
	{"/bin/busybox", "sh", "-c"},
	{"/busybox/sh", "-c"},
}

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

// runScript runs script once, with the first of shells that runs in the
// container, and returns its exit code and error output. The error is set only
// when the script didn't run to an exit code: no shell ran, the runtime
// failed, or the command timed out.
//
// timeout bounds the whole action, finding the shell included. It is a
// deadline on ctx, which the CRI call inherits. Each command still gets the
// full timeout for the runtime to stop it: CRI takes it in whole seconds, and
// the time left after the shell is found would round down to 0, which means
// no limit.
func runScript(ctx context.Context, e Execer, containerID, script, filePath string, timeout time.Duration) (int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	shell, err := findShell(ctx, e, containerID, timeout)
	if err != nil {
		return 0, "", err
	}
	cmd := slices.Concat(shell, []string{script, "sh", filePath})
	_, stderr, err := e.ExecSync(ctx, containerID, cmd, timeout)
	if err == nil {
		return 0, "", nil
	}
	var exit interface{ ExitStatus() int }
	if errors.As(err, &exit) {
		return exit.ExitStatus(), output(stderr), nil
	}
	return 0, "", fmt.Errorf("failed to run the file action on %s in container %s: %w", filePath, containerID, err)
}

// findShell returns the first of shells that runs a no-op in the container,
// as the container's user. The no-op is safe to repeat, so any failure moves
// on to the next shell, and the script itself runs only once. A timeout stops
// the search, so a hung runtime costs one timeout, not one per shell.
func findShell(ctx context.Context, e Execer, containerID string, timeout time.Duration) ([]string, error) {
	var errs []error
	for _, shell := range shells {
		_, _, err := e.ExecSync(ctx, containerID, slices.Concat(shell, []string{"exit 0"}), timeout)
		if err == nil {
			return shell, nil
		}
		if errors.Is(err, remote.ErrCommandTimedOut) {
			return nil, fmt.Errorf("failed to run %s in container %s: %w", shell[0], containerID, err)
		}
		errs = append(errs, fmt.Errorf("%s: %w", shell[0], err))
	}
	return nil, fmt.Errorf("failed to run a shell in container %s; file actions need /bin/sh, /usr/bin/sh, /bin/busybox or /busybox/sh in the image: %w",
		containerID, errors.Join(errs...))
}

func output(b []byte) string {
	if len(b) > maxOutput {
		b = b[:maxOutput]
	}
	return strings.TrimSpace(string(b))
}

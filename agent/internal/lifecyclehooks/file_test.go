// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecyclehooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	remote "k8s.io/cri-client/pkg"
)

// localShell runs the command with the local /bin/sh, as a container runtime
// would run it in the container, and reports a non-zero exit the way
// cri-client does: an error with an ExitStatus method.
type localShell struct {
	cmds [][]string
}

type exitError int

func (e exitError) Error() string   { return fmt.Sprintf("exited with %d", int(e)) }
func (e exitError) ExitStatus() int { return int(e) }

func (s *localShell) ExecSync(ctx context.Context, _ string, cmd []string, timeout time.Duration) ([]byte, []byte, error) {
	s.cmds = append(s.cmds, cmd)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	c.Stdout, c.Stderr = &stdout, &stderr
	err := c.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return stdout.Bytes(), stderr.Bytes(), exitError(exit.ExitCode())
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

// oneShell stands for an image with only one of the shells. A command for
// any other one fails with missing, as the runtime reports a missing binary;
// that shell runs as the local /bin/sh.
type oneShell struct {
	path    string
	missing error
	cmds    [][]string
}

func (s *oneShell) ExecSync(ctx context.Context, id string, cmd []string, timeout time.Duration) ([]byte, []byte, error) {
	s.cmds = append(s.cmds, cmd)
	if cmd[0] != s.path {
		return nil, nil, s.missing
	}
	local := append([]string{"/bin/sh"}, cmd[slices.Index(cmd, "-c"):]...)
	return (&localShell{}).ExecSync(ctx, id, local, timeout)
}

// failingExec stands for a runtime that couldn't run the command.
type failingExec struct{ err error }

func (f failingExec) ExecSync(context.Context, string, []string, time.Duration) ([]byte, []byte, error) {
	return nil, nil, f.err
}

func TestFileExists(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "it's ready")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(file, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		path    string
		want    bool
		wantErr bool
	}{
		{"regular file, with a quote and a space in its name", file, true, false},
		{"symlink to a regular file", filepath.Join(dir, "link"), true, false},
		{"missing", filepath.Join(dir, "missing"), false, false},
		{"missing parent", filepath.Join(dir, "no", "such"), false, false},
		{"directory", dir, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shell := &localShell{}
			got, err := FileExists(t.Context(), shell, "ctr", tc.path, 5*time.Second)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("FileExists = %v, %v; want %v, error %v", got, err, tc.want, tc.wantErr)
			}
			// The path is an argument, never part of the script.
			if cmd := shell.cmds[len(shell.cmds)-1]; cmd[len(cmd)-1] != tc.path || strings.Contains(cmd[2], tc.path) {
				t.Fatalf("command %q doesn't pass the path as its last argument", cmd)
			}
		})
	}
}

func TestCreateMarker(t *testing.T) {
	t.Run("new file", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "released")
		if err := CreateMarker(t.Context(), &localShell{}, "ctr", file, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(file)
		if err != nil || !info.Mode().IsRegular() || info.Size() != 0 {
			t.Fatalf("marker = %v, %v; want an empty regular file", info, err)
		}
	})
	t.Run("existing regular file", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "released")
		if err := os.WriteFile(file, []byte("app data"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := CreateMarker(t.Context(), &localShell{}, "ctr", file, 5*time.Second)
		if !errors.Is(err, fs.ErrExist) {
			t.Fatalf("CreateMarker = %v, want fs.ErrExist", err)
		}
		if data, _ := os.ReadFile(file); string(data) != "app data" {
			t.Fatalf("the existing file was changed to %q", data)
		}
	})
	t.Run("existing directory", func(t *testing.T) {
		err := CreateMarker(t.Context(), &localShell{}, "ctr", t.TempDir(), 5*time.Second)
		if err == nil || errors.Is(err, fs.ErrExist) {
			t.Fatalf("CreateMarker = %v, want an error that isn't fs.ErrExist", err)
		}
	})
	t.Run("missing parent", func(t *testing.T) {
		dir := t.TempDir()
		err := CreateMarker(t.Context(), &localShell{}, "ctr", filepath.Join(dir, "no", "released"), 5*time.Second)
		if err == nil || !strings.Contains(err.Error(), "failed to create") {
			t.Fatalf("CreateMarker = %v, want a failure to create", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "no")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatal("CreateMarker created the parent directory")
		}
	})
}

func TestFileActionsReportARuntimeThatCouldNotRunThem(t *testing.T) {
	timedOut := fmt.Errorf("%w: after 1s", remote.ErrCommandTimedOut)
	_, err := FileExists(t.Context(), failingExec{timedOut}, "ctr", "/ready", time.Second)
	if !errors.Is(err, remote.ErrCommandTimedOut) {
		t.Fatalf("FileExists = %v, want the timeout", err)
	}
	err = CreateMarker(t.Context(), failingExec{errors.New(`exec: "/bin/sh": not found`)}, "ctr", "/released", time.Second)
	if err == nil || !strings.Contains(err.Error(), "file actions need /bin/sh") {
		t.Fatalf("CreateMarker = %v, want a hint about the missing shell", err)
	}
}

// lostReply stands for a runtime whose reply to the script is lost, after the
// script may have run. The no-op that finds the shell succeeds.
type lostReply struct{ scripts int }

func (l *lostReply) ExecSync(_ context.Context, _ string, cmd []string, _ time.Duration) ([]byte, []byte, error) {
	if cmd[len(cmd)-1] == "exit 0" {
		return nil, nil, nil
	}
	l.scripts++
	return nil, nil, errors.New("rpc error: code = Unavailable")
}

func TestFileActionsRunTheScriptOnce(t *testing.T) {
	runtime := &lostReply{}
	if err := CreateMarker(t.Context(), runtime, "ctr", "/released", time.Second); err == nil || runtime.scripts != 1 {
		t.Fatalf("CreateMarker = %v after %d runs of the script, want an error after one", err, runtime.scripts)
	}
}

// slowRuntime takes delay for every command. Like cri-client, it gives up
// when ctx ends and reports that as a timeout.
type slowRuntime struct {
	delay    time.Duration
	timeouts []time.Duration
}

func (r *slowRuntime) ExecSync(ctx context.Context, _ string, _ []string, timeout time.Duration) ([]byte, []byte, error) {
	r.timeouts = append(r.timeouts, timeout)
	select {
	case <-time.After(r.delay):
		return nil, nil, nil
	case <-ctx.Done():
		return nil, nil, fmt.Errorf("%w: %w", remote.ErrCommandTimedOut, ctx.Err())
	}
}

func TestFileActionsKeepToOneTimeout(t *testing.T) {
	// The no-op and the script each fit the timeout, but not both together.
	runtime := &slowRuntime{delay: 300 * time.Millisecond}
	timeout := 500 * time.Millisecond
	start := time.Now()
	err := CreateMarker(t.Context(), runtime, "ctr", "/released", timeout)
	if elapsed := time.Since(start); !errors.Is(err, remote.ErrCommandTimedOut) || elapsed > timeout+200*time.Millisecond {
		t.Fatalf("CreateMarker = %v after %s, want a timeout after about %s", err, elapsed, timeout)
	}
	// The runtime still gets the whole timeout for each command: CRI takes
	// it in whole seconds, so a shorter one could round down to no limit.
	if !slices.Equal(runtime.timeouts, []time.Duration{timeout, timeout}) {
		t.Fatalf("commands got timeouts %v, want %s for the no-op and the script", runtime.timeouts, timeout)
	}
}

func TestFileActionsFindTheShellInTheImage(t *testing.T) {
	missing := map[string]error{
		// containerd fails the exec itself; a runtime may also report the
		// shell's own exit code for a command it can't find.
		"exec error": errors.New("no such file or directory"),
		"exit 127":   exitError(127),
	}
	for _, shell := range shells {
		for how, err := range missing {
			t.Run(shell[0]+" after "+how, func(t *testing.T) {
				file := filepath.Join(t.TempDir(), "released")
				image := &oneShell{path: shell[0], missing: err}
				if err := CreateMarker(t.Context(), image, "ctr", file, 5*time.Second); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(file); err != nil {
					t.Fatalf("the marker wasn't created: %v", err)
				}
				// The no-op found the shell, and the script ran once, with it.
				last := image.cmds[len(image.cmds)-1]
				if last[0] != shell[0] || last[len(last)-1] != file {
					t.Fatalf("the last command was %q, want the script with %s", last, shell[0])
				}
				if noop := image.cmds[len(image.cmds)-2]; noop[0] != shell[0] || noop[len(noop)-1] != "exit 0" {
					t.Fatalf("the command before the script was %q, want the no-op with %s", noop, shell[0])
				}
			})
		}
	}
}

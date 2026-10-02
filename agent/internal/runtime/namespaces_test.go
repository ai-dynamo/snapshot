// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestCommandInNamespacesPassesOpenFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("TEST_CONTAINER_ROOT", dir)
	script := `#!/bin/sh
set -eu
while [ "$1" != -- ]; do
    case "$1" in
        --mount=*) test "$(readlink "${1#*=}")" = "$(readlink /proc/self/ns/mnt)" ;;
        --root=*|--wd=*) test "$(readlink "${1#*=}")" = "$TEST_CONTAINER_ROOT" ;;
        -t) shift ;;
        -u|-i|-n|-p) ;;
        *) exit 1 ;;
    esac
    shift
done
shift
exec "$@"
`
	binary := filepath.Join(dir, "worker")
	for path, contents := range map[string]string{
		filepath.Join(dir, "nsenter"):  script,
		binary:                         "#!/bin/sh\ncat \"$1\"\n",
		filepath.Join(dir, "artifact"): "checkpoint contents",
	} {
		if err := os.WriteFile(path, []byte(contents), 0700); err != nil {
			t.Fatal(err)
		}
	}
	mountNS, err := os.Open("/proc/self/ns/mnt")
	if err != nil {
		t.Fatal(err)
	}
	defer mountNS.Close()
	cmd, closeFiles, err := CommandInNamespaces(context.Background(), os.Getpid(), mountNS, dir, binary)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFiles()
	// The child must execute the already-open binary, not resolve its old path.
	if err := os.Rename(binary, binary+".moved"); err != nil {
		t.Fatal(err)
	}
	artifact, err := os.Open(filepath.Join(dir, "artifact"))
	if err != nil {
		t.Fatal(err)
	}
	defer artifact.Close()
	cmd.Args = append(cmd.Args, InheritFile(cmd, artifact))
	output, err := cmd.CombinedOutput()
	if err != nil || string(output) != "checkpoint contents" {
		t.Fatalf("namespace command: %v, output %q", err, output)
	}
}

func TestCommandInNamespacesCancellationKillsForkedChild(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	childPath := filepath.Join(dir, "child-pid")
	t.Setenv("TEST_CHILD_PID", childPath)
	// nsenter forks when entering a PID namespace. The child inherits the output pipes.
	// Killing only nsenter leaves the worker running and the output wait blocked.
	script := `#!/bin/sh
set -eu
while [ "$1" != -- ]; do shift; done
shift
"$@" &
echo "$!" > "$TEST_CHILD_PID"
wait
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "nsenter"), []byte(script), 0700))
	mountNS, err := os.Open("/proc/self/ns/mnt")
	require.NoError(t, err)
	defer mountNS.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd, closeFiles, err := CommandInNamespaces(ctx, os.Getpid(), mountNS, dir, "/bin/sleep")
	require.NoError(t, err)
	defer closeFiles()
	cmd.Args = append(cmd.Args, "300")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	require.NoError(t, cmd.Start())
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()

	var childPID int
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(childPath)
		if err != nil {
			return false
		}
		childPID, err = strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil && childPID > 0
	}, 3*time.Second, 10*time.Millisecond)
	// A pidfd reports process exit before the parent reaps the child. It also lets cleanup
	// stop the child if cancellation fails.
	childFD, err := unix.PidfdOpen(childPID, 0)
	require.NoError(t, err)
	defer unix.Close(childFD)
	defer func() { _ = unix.PidfdSendSignal(childFD, unix.SIGKILL, nil, 0) }()

	cancel()
	select {
	case err := <-finished:
		require.Error(t, err)
	case <-time.After(helperWaitDelay + time.Second):
		t.Fatal("namespace command waited for the child's output pipes after cancellation")
	}
	fds := []unix.PollFd{{Fd: int32(childFD), Events: unix.POLLIN}}
	_, err = unix.Poll(fds, 1000)
	require.NoError(t, err)
	require.NotZero(t, fds[0].Revents&unix.POLLIN, "forked child survived cancellation")
}

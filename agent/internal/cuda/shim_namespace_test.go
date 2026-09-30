// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
)

func TestRestoreActionsInheritNamespaceProcessGroup(t *testing.T) {
	// Re-execute this test as nsrestore so the real CUDA command runner starts
	// its helper below the namespace launcher, with an independent Go context.
	if action := os.Getenv("TEST_NSRESTORE_ACTION"); action != "" {
		helper := os.Getenv("TEST_NSRESTORE_HELPER")
		var err error
		if action == actionRestore {
			err = restoreProcess(context.Background(), 11, "", helper, logr.Discard())
		} else {
			err = unlock(context.Background(), 11, helper, logr.Discard())
		}
		require.NoError(t, err)
		return
	}
	for _, action := range []string{actionRestore, actionUnlock} {
		t.Run(action, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
			t.Setenv("TEST_NSRESTORE_ACTION", action)
			pidPath := filepath.Join(dir, "helper-pid")
			t.Setenv("TEST_CUDA_HELPER_PID", pidPath)
			installFakeCUDAHelper(t, "echo $$ > \"$TEST_CUDA_HELPER_PID\"\nexec sleep 300\n")
			t.Setenv("TEST_NSRESTORE_HELPER", cudaCheckpointHelperBinary)
			script := "#!/bin/sh\nwhile [ \"$1\" != -- ]; do shift; done\nshift\n\"$@\" &\nwait\n"
			require.NoError(t, os.WriteFile(filepath.Join(dir, "nsenter"), []byte(script), 0700))
			binary, err := os.Executable()
			require.NoError(t, err)
			mountNS, err := os.Open("/proc/self/ns/mnt")
			require.NoError(t, err)
			defer mountNS.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cmd, closeFiles, err := snapshotruntime.CommandInNamespaces(ctx, os.Getpid(), mountNS, dir, binary)
			require.NoError(t, err)
			defer closeFiles()
			cmd.Args = append(cmd.Args, "-test.run=^TestRestoreActionsInheritNamespaceProcessGroup$")
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			require.NoError(t, cmd.Start())
			finished := make(chan error, 1)
			go func() { finished <- cmd.Wait() }()

			var helperPID int
			require.Eventually(t, func() bool {
				data, err := os.ReadFile(pidPath)
				if err != nil {
					return false
				}
				helperPID, err = strconv.Atoi(strings.TrimSpace(string(data)))
				return err == nil && helperPID > 0
			}, 3*time.Second, 10*time.Millisecond)
			helperFD, err := unix.PidfdOpen(helperPID, 0)
			require.NoError(t, err)
			defer unix.Close(helperFD)
			defer unix.PidfdSendSignal(helperFD, unix.SIGKILL, nil, 0)
			group, err := unix.Getpgid(helperPID)
			require.NoError(t, err)
			require.Equal(t, cmd.Process.Pid, group, "native helper escaped the namespace command's group")

			cancel()
			select {
			case err := <-finished:
				require.Error(t, err)
			case <-time.After(3 * time.Second):
				t.Fatal("namespace cancellation waited for the native helper's output pipes")
			}
			fds := []unix.PollFd{{Fd: int32(helperFD), Events: unix.POLLIN}}
			_, err = unix.Poll(fds, 1000)
			require.NoError(t, err)
			require.NotZero(t, fds[0].Revents&unix.POLLIN, "native helper survived namespace cancellation")
		})
	}
}

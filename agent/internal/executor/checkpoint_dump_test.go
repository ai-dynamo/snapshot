// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	criurpc "github.com/checkpoint-restore/go-criu/v8/rpc"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

// Exercise the real go-criu notification exchange and overlay writer. The worker
// removes the source filesystem before returning dump success, as a runtime may
// do after the source exits; copying the overlay after ExecuteDump is too late.
func TestDumpCheckpointPostDump(t *testing.T) {
	for _, mode := range []string{"success", "overlay failure", "dump failure", "no overlay"} {
		t.Run(mode, func(t *testing.T) {
			dir, upper := t.TempDir(), t.TempDir()
			writeOverlayTestFile(t, upper, "root/kept.txt", "checkpoint state")
			writeOverlayTestDump(t, dir, newOverlayTestRegFile(1, "/root/kept.txt", overlayTestRootMount))
			if mode == "overlay failure" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "files.img"), []byte("corrupt"), 0o600))
			}
			t.Setenv("SNAPSHOT_TEST_DUMP_WORKER", mode)
			t.Setenv("SNAPSHOT_TEST_DUMP_DIR", dir)
			t.Setenv("SNAPSHOT_TEST_DUMP_UPPER", upper)
			binary, err := os.Executable()
			require.NoError(t, err)
			wrapper := filepath.Join(t.TempDir(), "criu")
			script := "#!/bin/sh\nexec '" + strings.ReplaceAll(binary, "'", "'\\''") + "' -test.run=^TestDumpCheckpointWorker$ -- \"$@\"\n"
			require.NoError(t, os.WriteFile(wrapper, []byte(script), 0o700))
			state := &types.CheckpointContainerSnapshot{UpperDir: upper}
			if mode == "no overlay" {
				state.UpperDir = ""
			}
			timings := &checkpointPhaseTimings{}
			manifest := &types.CheckpointManifest{Artifact: types.ArtifactManifest{ContentUID: "content", ContainerName: "main"}}
			err = dumpCheckpoint(context.Background(), &criurpc.CriuOpts{}, &types.CRIUSettings{BinaryPath: wrapper},
				manifest, state, dir, timings, logr.Discard())
			switch mode {
			case "overlay failure":
				require.Error(t, err)
				assert.Equal(t, StepCaptureOverlay, StepOf(err))
				assert.FileExists(t, filepath.Join(dir, "hook-rejected"))
				assert.NoFileExists(t, filepath.Join(dir, "manifest.yaml"))
			case "dump failure":
				require.Error(t, err)
				assert.Equal(t, StepDumpCRIU, StepOf(err))
				assert.Zero(t, timings.OverlayCaptureDuration)
			default:
				require.NoError(t, err)
				assert.FileExists(t, filepath.Join(dir, "hook-acknowledged"))
				assert.Positive(t, timings.CRIUDumpDuration)
				if mode == "success" {
					assert.NoDirExists(t, upper)
					assert.Contains(t, overlayTestArchiveMembers(t, filepath.Join(dir, "rootfs-diff.tar")), "./root/kept.txt")
					assert.Positive(t, timings.OverlayCaptureDuration)
				} else {
					assert.Zero(t, timings.OverlayCaptureDuration)
				}
			}
		})
	}
}

// TestDumpCheckpointWorker is a subprocess speaking CRIU's SOCK_SEQPACKET RPC.
// It checks that the overlay exists when post-dump is acknowledged, rather than
// testing the callback in isolation and missing a notification wiring regression.
func TestDumpCheckpointWorker(t *testing.T) {
	mode := os.Getenv("SNAPSHOT_TEST_DUMP_WORKER")
	if mode == "" {
		t.Skip("CRIU RPC subprocess")
	}
	if os.Args[len(os.Args)-1] == "--version" {
		_, err := os.Stdout.WriteString("Version: test\n")
		require.NoError(t, err)
		return
	}
	fd, err := strconv.Atoi(os.Args[len(os.Args)-1])
	require.NoError(t, err)
	read := func() (*criurpc.CriuReq, error) {
		b := make([]byte, 65536)
		n, err := unix.Read(fd, b)
		if err != nil || n == 0 {
			return nil, err
		}
		req := &criurpc.CriuReq{}
		return req, req.UnmarshalVT(b[:n])
	}
	write := func(resp *criurpc.CriuResp) {
		b, err := resp.MarshalVT()
		require.NoError(t, err)
		_, err = unix.Write(fd, b)
		require.NoError(t, err)
	}
	req, err := read()
	require.NoError(t, err)
	require.NotNil(t, req)
	require.Equal(t, criurpc.CriuReqType_DUMP, req.GetType())
	require.True(t, req.GetOpts().GetNotifyScripts())
	if mode == "dump failure" {
		write(&criurpc.CriuResp{Type: criurpc.CriuReqType_DUMP.Enum(), Success: proto.Bool(false), CrErrno: proto.Int32(int32(unix.ENOSPC))})
		return
	}
	write(&criurpc.CriuResp{Type: criurpc.CriuReqType_NOTIFY.Enum(), Success: proto.Bool(true),
		Notify: &criurpc.CriuNotify{Script: proto.String("post-dump")}})
	req, err = read()
	dir := os.Getenv("SNAPSHOT_TEST_DUMP_DIR")
	if mode == "overlay failure" {
		require.Nil(t, req, "a failed callback must not acknowledge success")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "hook-rejected"), nil, 0o600))
		return
	}
	require.NoError(t, err)
	require.NotNil(t, req)
	require.True(t, req.GetNotifySuccess())
	if mode == "success" {
		require.FileExists(t, filepath.Join(dir, "rootfs-diff.tar"))
		require.NoError(t, os.RemoveAll(os.Getenv("SNAPSHOT_TEST_DUMP_UPPER")))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hook-acknowledged"), nil, 0o600))
	write(&criurpc.CriuResp{Type: criurpc.CriuReqType_DUMP.Enum(), Success: proto.Bool(true)})
}

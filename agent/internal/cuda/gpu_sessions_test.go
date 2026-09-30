// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
)

func helperScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

const fakeHelperStartup = `
case "$1" in
--daemon) printf 'ready\n' >&3; exec sleep 300 ;;
--wait-ready) read -r ready <&3; test "$ready" = ready; printf '{"custom_storage_available":true}\n' ;;
`

func startTestHelper(t *testing.T, commands string) *Helper {
	t.Helper()
	binary := helperScript(t, fakeHelperStartup+commands+"\nesac\n")
	helper, err := StartHelper(context.Background(), binary, types.CUDACheckpointSpec{Enabled: true}, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(helper.Stop)
	return helper
}

func TestHelperStartupWaitsForReadyAndRejectsProcessFailure(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run(strconv.FormatBool(ready), func(t *testing.T) {
			behavior := "exit 7"
			if ready {
				behavior = "printf 'ready\\n' >&3; exec sleep 300"
			}
			binary := helperScript(t, fmt.Sprintf(`case "$1" in
--daemon) %s ;;
--wait-ready) read -r ready <&3; test "$ready" = ready; printf '{"custom_storage_available":false}\n' ;;
esac
`, behavior))
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			helper, err := StartHelper(ctx, binary, types.CUDACheckpointSpec{}, logr.Discard())
			if (err == nil) != ready {
				t.Fatalf("ready=%t: helper=%v err=%v", ready, helper, err)
			}
			if ready {
				helper.Stop()
				select {
				case <-helper.Done():
				default:
					t.Fatal("Stop returned before daemon was reaped")
				}
			}
		})
	}
}

func TestHelperStartupBlocksUntilDaemonReady(t *testing.T) {
	entered, release := filepath.Join(t.TempDir(), "entered"), filepath.Join(t.TempDir(), "release")
	t.Setenv("HELPER_TEST_ENTERED", entered)
	t.Setenv("HELPER_TEST_RELEASE", release)
	binary := helperScript(t, `case "$1" in
--daemon) touch "$HELPER_TEST_ENTERED"; while [ ! -e "$HELPER_TEST_RELEASE" ]; do sleep 0.01; done; printf 'ready\n' >&3; exec sleep 300 ;;
--wait-ready) read -r ready <&3; test "$ready" = ready; printf '{"custom_storage_available":false}\n' ;;
esac
`)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	type result struct {
		helper *Helper
		err    error
	}
	done := make(chan result, 1)
	go func() {
		helper, err := StartHelper(ctx, binary, types.CUDACheckpointSpec{}, logr.Discard())
		done <- result{helper, err}
	}()
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("daemon did not start")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case result := <-done:
		result.helper.Stop()
		t.Fatalf("startup returned before READY: %v", result.err)
	default:
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	outcome := <-done
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	outcome.helper.Stop()
}

func TestHelperCustomStorageRequiresStartupCapability(t *testing.T) {
	binary := helperScript(t, `case "$1" in
--daemon) exec sleep 300 ;;
--wait-ready) printf '{"custom_storage_available":false}\n' ;;
esac
`)
	if _, err := StartHelper(context.Background(), binary, types.CUDACheckpointSpec{Enabled: true, StorageMode: "custom"}, logr.Discard()); err == nil || !strings.Contains(err.Error(), "does not support CustomStorage") {
		t.Fatalf("custom startup result: %v", err)
	}
}

func TestBindGPUSessionsPIDBounds(t *testing.T) {
	for _, tc := range []struct{ container, pid int }{
		{-1, 12}, {0, 12}, {1, 12}, {math.MaxInt32 + 1, 12}, {1<<32 + 34, 12},
		{34, -1}, {34, 0}, {34, math.MaxInt32 + 1}, {34, 1<<32 + 12},
	} {
		if sessions, err := BindGPUSessions(context.Background(), nil, "", tc.container, []int{tc.pid}, nil, "", false, false, false); err == nil || sessions != nil || !strings.Contains(err.Error(), "GPU") {
			t.Fatalf("accepted container=%d, pid=%d: %v", tc.container, tc.pid, err)
		}
	}
}

func TestBindGPUSessionsStorageModesAndDrain(t *testing.T) {
	for _, custom := range []bool{false, true} {
		for _, checksum := range []bool{false, true} {
			for _, save := range []bool{false, true} {
				t.Run(fmt.Sprintf("custom=%t/checksum=%t/save=%t", custom, checksum, save), func(t *testing.T) {
					argsPath := filepath.Join(t.TempDir(), "args")
					t.Setenv("HELPER_TEST_ARGS", argsPath)
					helper := startTestHelper(t, `
--bind-batch) test -d /proc/self/fd/4; printf '%s\n' "$@" > "$HELPER_TEST_ARGS" ;;
--drain-batch) exit 0 ;;
`)
					sessions, err := BindGPUSessions(context.Background(), helper, t.TempDir(), math.MaxInt32,
						[]int{math.MaxInt32}, []string{"GPU-one"}, "old=new", save, custom, checksum)
					if err != nil {
						t.Fatal(err)
					}
					data, err := os.ReadFile(argsPath)
					if err != nil {
						t.Fatal(err)
					}
					args := string(data)
					if strings.Contains(args, "--checksum\n") != (custom && checksum) ||
						strings.Contains(args, "--storage-mode\ncustom\n") != custom ||
						strings.Contains(args, "--direction\nsave\n") != save ||
						!strings.Contains(args, "--session\n1:2147483647:5:6\n") {
						t.Fatalf("unexpected bind args: %s", args)
					}
					if err := helper.Drain(sessions); err != nil || len(sessions) != 0 {
						t.Fatalf("drain: %v, sessions=%v", err, sessions)
					}
				})
			}
		}
	}
}

func TestHelperDrainFailureReapsProcessBeforeReturning(t *testing.T) {
	helper := startTestHelper(t, "--bind-batch) exit 0 ;;\n--drain-batch) exit 9 ;;\n")
	sessions, err := BindGPUSessions(context.Background(), helper, t.TempDir(), 34, []int{12}, nil, "", false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Drain(sessions); err == nil {
		t.Fatal("failed drain returned success")
	}
	select {
	case <-helper.Done():
	default:
		t.Fatal("drain failed before daemon exit was reaped")
	}
	if err := syscall.Kill(helper.cmd.Process.Pid, 0); err != syscall.ESRCH {
		t.Fatalf("daemon remains after drain failure: %v", err)
	}
}

func TestHelperDrainWaitsForJoinAcknowledgement(t *testing.T) {
	entered, release := filepath.Join(t.TempDir(), "entered"), filepath.Join(t.TempDir(), "release")
	t.Setenv("HELPER_TEST_ENTERED", entered)
	t.Setenv("HELPER_TEST_RELEASE", release)
	helper := startTestHelper(t, `
--bind-batch) exit 0 ;;
--drain-batch) touch "$HELPER_TEST_ENTERED"; while [ ! -e "$HELPER_TEST_RELEASE" ]; do sleep 0.01; done ;;
`)
	sessions, err := BindGPUSessions(context.Background(), helper, t.TempDir(), 34, []int{12}, nil, "", false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- helper.Drain(sessions) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("drain CLI did not start")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("Drain returned before join acknowledgement: %v", err)
	default:
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRunGPUSessionsCancellationReachesDaemon(t *testing.T) {
	client, peer, err := helperSocketPair()
	if err != nil {
		t.Fatal(err)
	}
	sessions := GPUSessions{"12": client}
	defer sessions.Close()
	defer peer.Close()
	conn, err := net.FileConn(peer)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	binary := helperScript(t, "printf x >&3\nexec sleep 300\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunGPUSessions(ctx, binary, sessions, []int{12}, false, logr.Discard(), []int{112}) }()
	var byte [1]byte
	if _, err := io.ReadFull(conn, byte[:]); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := conn.Read(byte[:]); err != io.EOF {
		t.Fatalf("cancellation did not half-close retained endpoint: %v", err)
	}
	if err := <-done; err == nil {
		t.Fatal("cancelled batch succeeded")
	}
}

func TestRunGPUSessionsCLIArgumentsAndNumericTelemetry(t *testing.T) {
	client, peer, err := helperSocketPair()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	sessions := GPUSessions{"12": client}
	defer sessions.Close()
	binary := helperScript(t, `test "$1" = --run-batch
test "$2" = --direction
test "$3" = load
test "$4" = --session
test "$5" = 12:3:112
printf '{"pid":12,"operation":"TRANSFER","reply":{"metrics":{"bytes":"512","transfer_seconds":0.25,"transfer_start_ns":"123"}}}\n'
printf 'malformed diagnostic\n'
`)
	var logs []map[string]any
	log := funcr.NewJSON(func(raw string) {
		var fields map[string]any
		if err := json.Unmarshal([]byte(raw), &fields); err != nil {
			t.Fatal(err)
		}
		logs = append(logs, fields)
	}, funcr.Options{})
	if err := RunGPUSessions(context.Background(), binary, sessions, []int{12}, false, log, []int{112}); err != nil {
		t.Fatal(err)
	}
	if len(logs) < 1 || logs[0]["bytes"] != float64(512) || logs[0]["transfer_seconds"] != 0.25 || logs[0]["transfer_start_ns"] != float64(123) {
		t.Fatalf("metrics are not numeric JSON: %v", logs)
	}
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
)

func exitStatusError(t *testing.T, script string) error {
	t.Helper()
	err := exec.Command("sh", "-c", script).Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("sh -c %q = %v, want exit error", script, err)
	}
	return err
}

func nsrestoreOutputOf(t *testing.T, stdout, stderr string) *nsrestoreOutput {
	t.Helper()
	output := newNSRestoreOutput(io.Discard)
	cmd := exec.Command("sh", "-c", `printf '%s' "$1"; printf '%s' "$2" >&2`, "sh", stdout, stderr)
	output.Attach(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return output
}

func TestNSRestoreOutputFailure(t *testing.T) {
	nsenterNotFound := "nsenter: failed to execute /proc/self/fd/4: No such file or directory\n"
	infoLine := "2026-10-19T17:35:54.362Z\tINFO\tnsrestore\tnsrestore/main.go:76\tStarting nsrestore workflow\n"
	errorLine := "2026-10-19T17:35:54.363Z\tERROR\tnsrestore\tnsrestore/main.go:82\trestore failed\t" +
		`{"error": "rootfs diff failed: tar extract failed: exit status 2"}` + "\n" +
		"main.main\n\t/workspace/agent/cmd/nsrestore/main.go:82\n"
	tests := []struct {
		name   string
		stdout string
		stderr string
		runErr error
		ctxErr error
		want   string
	}{
		{
			name:   "reported cause wins",
			stdout: `{"restoredPID":0,"error":"CRIU restore failed: mnt-v2: Can't stat mountpoint /cache/vllm"}` + "\n",
			stderr: infoLine + errorLine,
			runErr: exitStatusError(t, "exit 1"),
			want:   "CRIU restore failed: mnt-v2: Can't stat mountpoint /cache/vllm",
		},
		{
			name:   "reported step is kept without a second prefix",
			stdout: `{"error":"while applying rootfs diff: rootfs diff failed: tar: boom","step":"applying rootfs diff"}` + "\n",
			runErr: exitStatusError(t, "exit 1"),
			want:   "while applying rootfs diff: rootfs diff failed: tar: boom",
		},
		{
			name:   "reported step is attached when the text lacks it",
			stdout: `{"error":"CRIU restore failed: boom","step":"restoring with CRIU"}` + "\n",
			runErr: exitStatusError(t, "exit 1"),
			want:   "while restoring with CRIU: CRIU restore failed: boom",
		},
		{
			name:   "reported step keeps the timeout suffix",
			stdout: `{"error":"while restoring with CRIU: CRIU restore failed: context canceled","step":"restoring with CRIU"}` + "\n",
			runErr: exitStatusError(t, "exit 1"),
			ctxErr: context.DeadlineExceeded,
			want:   "while restoring with CRIU: CRIU restore failed: context canceled; the restore timeout expired",
		},
		{
			name:   "reported cause names the timeout",
			stdout: `{"restoredPID":0,"error":"CRIU restore failed: context canceled"}` + "\n",
			runErr: exitStatusError(t, "exit 1"),
			ctxErr: context.DeadlineExceeded,
			want:   "CRIU restore failed: context canceled; the restore timeout expired",
		},
		{
			name:   "reported cause names a cancelled restore",
			stdout: `{"restoredPID":0,"error":"CRIU restore failed: context canceled"}` + "\n",
			runErr: exitStatusError(t, "exit 1"),
			ctxErr: context.Canceled,
			want:   "CRIU restore failed: context canceled; the restore was cancelled",
		},
		{
			name:   "nsenter exec not found",
			stderr: nsenterNotFound,
			runErr: exitStatusError(t, "exit 127"),
			want:   "nsenter could not start nsrestore inside the placeholder (exit 127, executable not found): nsenter: failed to execute /proc/self/fd/4: No such file or directory",
		},
		{
			name:   "nsenter exec not executable",
			stderr: "nsenter: failed to execute /proc/self/fd/4: Permission denied\n",
			runErr: exitStatusError(t, "exit 126"),
			want:   "nsenter could not start nsrestore inside the placeholder (exit 126, not executable): nsenter: failed to execute /proc/self/fd/4: Permission denied",
		},
		{
			name:   "panic line beats the stack",
			stderr: infoLine + "panic: boom\n\ngoroutine 1 [running]:\nmain.main()\n",
			runErr: exitStatusError(t, "exit 2"),
			want:   "nsrestore exited with status 2 without reporting a cause: panic: boom",
		},
		{
			name:   "panic line survives a trace longer than the stderr tail",
			stderr: "panic: boom\n\ngoroutine 1 [running]:\n" + strings.Repeat("main.deep(...)\n\t/workspace/agent/cmd/nsrestore/main.go:99 +0x1f\n", 200),
			runErr: exitStatusError(t, "exit 2"),
			want:   "nsrestore exited with status 2 without reporting a cause: panic: boom",
		},
		{
			name:   "last error log line otherwise",
			stderr: infoLine + errorLine + infoLine,
			runErr: exitStatusError(t, "exit 1"),
			want:   "nsrestore exited with status 1 without reporting a cause: restore failed: rootfs diff failed: tar extract failed: exit status 2",
		},
		{
			name:   "error log line without fields",
			stderr: "2026-10-19T17:35:54.363Z\tERROR\tnsrestore\tnsrestore/main.go:85\tFailed to write restore result\nmain.main\n\t/workspace/agent/cmd/nsrestore/main.go:85\n",
			runErr: exitStatusError(t, "exit 1"),
			want:   "nsrestore exited with status 1 without reporting a cause: Failed to write restore result",
		},
		{
			name:   "info lines and stack frames are not a cause",
			stderr: infoLine + "main.main\n\t/workspace/agent/cmd/nsrestore/main.go:82\n",
			runErr: exitStatusError(t, "exit 1"),
			want:   "nsrestore exited with status 1 without reporting a cause",
		},
		{
			name:   "unstructured stdout is kept",
			stdout: "panic: boom",
			runErr: exitStatusError(t, "exit 1"),
			want:   "nsrestore exited with status 1 without reporting a cause: stdout: panic: boom",
		},
		{
			name:   "killed after the restore timeout",
			stderr: infoLine,
			runErr: exitStatusError(t, "kill -KILL $$"),
			ctxErr: context.DeadlineExceeded,
			want:   "nsrestore was killed (signal: killed); the restore timeout expired",
		},
		{
			name:   "killed without a timeout",
			stderr: infoLine,
			runErr: exitStatusError(t, "kill -KILL $$"),
			want:   "nsrestore was killed (signal: killed)",
		},
		{
			name:   "launch failure names a cancelled restore",
			runErr: errors.New(`exec: "nsenter": executable file not found in $PATH`),
			ctxErr: context.Canceled,
			want:   `launch nsrestore: exec: "nsenter": executable file not found in $PATH; the restore was cancelled`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := nsrestoreOutputOf(t, tt.stdout, tt.stderr).failure(tt.runErr, tt.ctxErr)
			if err == nil || err.Error() != tt.want {
				t.Fatalf("failure() = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestNSRestoreOutputFailureCarriesTheReportedStep(t *testing.T) {
	err := nsrestoreOutputOf(t, `{"error":"while restoring with CRIU: boom","step":"restoring with CRIU"}`, "").failure(exitStatusError(t, "exit 1"), context.DeadlineExceeded)
	if got := StepOf(err); got != StepRestoreCRIU {
		t.Fatalf("StepOf = %q, want %q (err %v)", got, StepRestoreCRIU, err)
	}
	if got := StepOf(nsrestoreOutputOf(t, `{"error":"boom"}`, "").failure(exitStatusError(t, "exit 1"), nil)); got != "" {
		t.Fatalf("StepOf(no step) = %q, want empty", got)
	}
}

func TestNSRestoreOutputFailureKeepsLaunchError(t *testing.T) {
	runErr := errors.New(`exec: "nsenter": executable file not found in $PATH`)
	err := nsrestoreOutputOf(t, "", "").failure(runErr, nil)
	if !errors.Is(err, runErr) || err.Error() != "launch nsrestore: "+runErr.Error() {
		t.Fatalf("failure(launch) = %v", err)
	}
}

func TestNSRestoreOutputResult(t *testing.T) {
	result, err := nsrestoreOutputOf(t, `{"restoredPID":4242}`+"\n", "").result()
	if err != nil || result.RestoredPID != 4242 {
		t.Fatalf("result() = %+v, %v", result, err)
	}
	if _, err := nsrestoreOutputOf(t, "not json", "").result(); err == nil || !strings.Contains(err.Error(), "failed to parse nsrestore result") {
		t.Fatalf("result(unparseable) = %v", err)
	}
	if _, err := nsrestoreOutputOf(t, `{"restoredPID":0}`, "").result(); err == nil || err.Error() != "nsrestore returned invalid PID 0" {
		t.Fatalf("result(no pid) = %v", err)
	}
}

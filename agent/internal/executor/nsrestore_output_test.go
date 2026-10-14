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
	logLine := `{"level":"info","msg":"Starting nsrestore workflow"}` + "\n"
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
			stderr: logLine,
			runErr: exitStatusError(t, "exit 1"),
			want:   "CRIU restore failed: mnt-v2: Can't stat mountpoint /cache/vllm",
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
			stderr: logLine + "panic: boom\n\ngoroutine 1 [running]:\nmain.main()\n",
			runErr: exitStatusError(t, "exit 2"),
			want:   "nsrestore exited with status 2 without reporting a cause: panic: boom",
		},
		{
			name:   "last stderr line otherwise",
			stderr: logLine + `{"level":"error","msg":"restore failed"}` + "\n",
			runErr: exitStatusError(t, "exit 1"),
			want:   `nsrestore exited with status 1 without reporting a cause: {"level":"error","msg":"restore failed"}`,
		},
		{
			name:   "unstructured stdout is kept",
			stdout: "panic: boom",
			runErr: exitStatusError(t, "exit 1"),
			want:   "nsrestore exited with status 1 without reporting a cause: stdout: panic: boom",
		},
		{
			name:   "killed after the restore timeout",
			runErr: exitStatusError(t, "kill -KILL $$"),
			ctxErr: context.DeadlineExceeded,
			want:   "nsrestore was killed (signal: killed) after the restore timeout expired",
		},
		{
			name:   "killed without a timeout",
			stderr: logLine,
			runErr: exitStatusError(t, "kill -KILL $$"),
			want:   `nsrestore was killed (signal: killed): {"level":"info","msg":"Starting nsrestore workflow"}`,
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

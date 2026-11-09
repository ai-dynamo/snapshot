// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"k8s.io/utils/ptr"

	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
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
			name:   "reported cause after the progress message",
			stdout: changingContainerLine + `{"error":"CRIU restore failed: boom","step":"restoring with CRIU"}` + "\n",
			runErr: exitStatusError(t, "exit 1"),
			want:   "while restoring with CRIU: CRIU restore failed: boom",
		},
		{
			name:   "killed right after the progress message",
			stdout: changingContainerLine,
			runErr: exitStatusError(t, "kill -KILL $$"),
			want:   "nsrestore was killed (signal: killed)",
		},
		{
			name:   "progress message is not quoted as stdout",
			stdout: changingContainerLine,
			runErr: exitStatusError(t, "exit 1"),
			want:   "nsrestore exited with status 1 without reporting a cause",
		},
		{
			name:   "other stdout is quoted without the progress message",
			stdout: changingContainerLine + "panic: boom\n",
			runErr: exitStatusError(t, "exit 1"),
			want:   "nsrestore exited with status 1 without reporting a cause: stdout: panic: boom",
		},
		{
			name:   "reported cause is the last line",
			stdout: "stray output\n" + `{"error":"CRIU restore failed: boom"}` + "\n",
			runErr: exitStatusError(t, "exit 1"),
			want:   "CRIU restore failed: boom",
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

const changingContainerLine = `{"changingContainer":true}` + "\n"

func TestNSRestoreOutputFailureMarksAChangedContainer(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		runErr error
		ctxErr error
		want   bool
	}{
		{name: "reported cause before any change", stdout: `{"error":"boom","step":"preparing CRIU restore"}` + "\n", runErr: exitStatusError(t, "exit 1")},
		{name: "reported cause after the change began", stdout: changingContainerLine + `{"error":"boom"}` + "\n", runErr: exitStatusError(t, "exit 1"), want: true},
		{name: "killed after the change began", stdout: changingContainerLine, runErr: exitStatusError(t, "kill -KILL $$"), want: true},
		{name: "timeout after the change began", stdout: changingContainerLine + `{"error":"boom"}` + "\n", runErr: exitStatusError(t, "exit 1"), ctxErr: context.DeadlineExceeded, want: true},
		{name: "progress that names no change", stdout: `{"changingContainer":false}` + "\n" + `{"error":"boom"}` + "\n", runErr: exitStatusError(t, "exit 1")},
		{name: "nsenter failed", runErr: exitStatusError(t, "exit 127")},
		{name: "launch failed", runErr: errors.New("exec: not found")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := nsrestoreOutputOf(t, tt.stdout, "").failure(tt.runErr, tt.ctxErr)
			if got := containerChanged(failStepUnlessNamed(StepRunNSRestore, err)); got != tt.want {
				t.Fatalf("containerChanged(%v) = %v, want %v", err, got, tt.want)
			}
		})
	}
}

func TestNSRestoreOutputFailureKeepsStepAndTimeoutOnAChangedContainer(t *testing.T) {
	err := nsrestoreOutputOf(t, changingContainerLine+`{"error":"boom","step":"restoring with CRIU"}`+"\n", "").
		failure(exitStatusError(t, "exit 1"), context.DeadlineExceeded)
	if got := StepOf(err); got != StepRestoreCRIU {
		t.Fatalf("StepOf = %q, want %q", got, StepRestoreCRIU)
	}
	if want := "while restoring with CRIU: boom; the restore timeout expired"; err.Error() != want {
		t.Fatalf("failure() = %q, want %q", err, want)
	}
}

func TestNSRestoreOutputFailureRebuildsThePageBrokerCode(t *testing.T) {
	const text = "while restoring CUDA state: restore CUDA: PageBroker ARTIFACT_CORRUPT: chunk 3 checksum mismatch"
	tests := []struct {
		name     string
		code     string
		wantCode *pagebroker.Failure_Code
	}{
		{name: "known code", code: "ARTIFACT_CORRUPT", wantCode: ptr.To(pagebroker.Failure_ARTIFACT_CORRUPT)},
		{name: "unspecified code", code: "UNSPECIFIED", wantCode: ptr.To(pagebroker.Failure_UNSPECIFIED)},
		{name: "no code"},
		{name: "code this agent does not know", code: "SOMETHING_NEW"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line, err := json.Marshal(RestoreInNamespaceResult{Error: text, Step: string(StepRestoreCUDA), Code: tt.code})
			if err != nil {
				t.Fatal(err)
			}
			got := nsrestoreOutputOf(t, string(line)+"\n", "").failure(exitStatusError(t, "exit 1"), nil)
			if got.Error() != text {
				t.Fatalf("failure() = %q, want %q", got, text)
			}
			if StepOf(got) != StepRestoreCUDA {
				t.Fatalf("StepOf = %q, want %q", StepOf(got), StepRestoreCUDA)
			}
			var failure *pagebroker.FailureError
			found := errors.As(got, &failure)
			if tt.wantCode == nil {
				if found {
					t.Fatalf("errors.As found PageBroker code %s, want none", failure.Code())
				}
				return
			}
			if !found || failure.Code() != *tt.wantCode {
				t.Fatalf("errors.As = %v, %v, want code %s", found, failure, *tt.wantCode)
			}
		})
	}
}

func TestNewRestoreInNamespaceFailureRoundTrip(t *testing.T) {
	brokerErr := failStep(StepRestoreCUDA, fmt.Errorf("restore CUDA: %w", pagebroker.NewFailureError(pagebroker.Failure_UNAVAILABLE, "too many GPU handlers")))
	tests := []struct {
		name     string
		err      error
		wantCode string
		wantStep Step
	}{
		{name: "PageBroker failure", err: brokerErr, wantCode: "UNAVAILABLE", wantStep: StepRestoreCUDA},
		{name: "CRIU failure", err: failStep(StepRestoreCRIU, errors.New("criu exited 1")), wantStep: StepRestoreCRIU},
		{name: "failure without a step", err: errors.New("boom")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := NewRestoreInNamespaceFailure(tt.err)
			if result.Code != tt.wantCode || Step(result.Step) != tt.wantStep || result.Error != tt.err.Error() {
				t.Fatalf("NewRestoreInNamespaceFailure() = %+v", result)
			}
			line, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			got := nsrestoreOutputOf(t, string(line)+"\n", "").failure(exitStatusError(t, "exit 1"), nil)
			if got.Error() != tt.err.Error() || StepOf(got) != tt.wantStep {
				t.Fatalf("agent read %q at step %q, want %q at %q", got, StepOf(got), tt.err, tt.wantStep)
			}
			var want, read *pagebroker.FailureError
			if errors.As(tt.err, &want) != errors.As(got, &read) || (want != nil && want.Code() != read.Code()) {
				t.Fatalf("PageBroker code did not survive: sent %v, read %v", want, read)
			}
		})
	}
}

func TestExecuteRestoreStopsWhenProgressCannotBeReported(t *testing.T) {
	reportErr := errors.New("write /dev/stdout: broken pipe")
	var reported []RestoreInNamespaceProgress
	opts := RestoreOptions{
		CheckpointPath: t.TempDir(),
		ReportProgress: func(progress RestoreInNamespaceProgress) error {
			reported = append(reported, progress)
			return reportErr
		},
	}
	_, _, _, err := executeRestore(context.Background(), nil, nil, opts, "", logr.Discard())
	if !errors.Is(err, reportErr) || StepOf(err) != StepApplyRootfsDiff {
		t.Fatalf("executeRestore() = %v, want the report failure at %q", err, StepApplyRootfsDiff)
	}
	if len(reported) != 1 || !reported[0].ChangingContainer {
		t.Fatalf("reported %+v, want one changingContainer message", reported)
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
	result, err = nsrestoreOutputOf(t, changingContainerLine+`{"restoredPID":4242}`+"\n", "").result()
	if err != nil || result.RestoredPID != 4242 {
		t.Fatalf("result(after progress) = %+v, %v", result, err)
	}
	_, err = nsrestoreOutputOf(t, changingContainerLine, "").result()
	if err == nil || strings.Contains(err.Error(), "changingContainer") {
		t.Fatalf("result(progress only) = %v, want a parse failure that does not quote the progress message", err)
	}
}

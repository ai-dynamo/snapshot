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

	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
)

// nsrestoreOutput reads nsrestore's result from stdout and, when the run
// fails, names the cause so restore status and events say what stopped
// rather than a bare exit status. nsenter's own line is preferred as the
// detail because it is what explains a launch that never reached nsrestore.
type nsrestoreOutput struct {
	snapshotruntime.ProcessOutput
}

func newNSRestoreOutput(mirror io.Writer) *nsrestoreOutput {
	return &nsrestoreOutput{ProcessOutput: snapshotruntime.NewProcessOutput(mirror, "nsenter:", "panic:", "fatal error:")}
}

// stdoutLines splits nsrestore's stdout into its progress messages and the
// other lines, the last of which is the result.
func (o *nsrestoreOutput) stdoutLines() (changingContainer bool, other []string) {
	for _, line := range strings.Split(string(o.Stdout()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var progress RestoreInNamespaceProgress
		if json.Unmarshal([]byte(line), &progress) == nil && progress.ChangingContainer {
			changingContainer = true
			continue
		}
		other = append(other, line)
	}
	return changingContainer, other
}

func (o *nsrestoreOutput) resultLine() string {
	_, other := o.stdoutLines()
	if len(other) == 0 {
		return ""
	}
	return other[len(other)-1]
}

func (o *nsrestoreOutput) result() (*RestoreInNamespaceResult, error) {
	var result RestoreInNamespaceResult
	if err := json.Unmarshal([]byte(o.resultLine()), &result); err != nil {
		_, other := o.stdoutLines()
		return nil, fmt.Errorf("failed to parse nsrestore result: %w\nstdout: %s", err, strings.Join(other, "\n"))
	}
	if result.RestoredPID <= 0 {
		return nil, fmt.Errorf("nsrestore returned invalid PID %d", result.RestoredPID)
	}
	return &result, nil
}

// failure prefers the cause nsrestore reports on stdout over the exit status.
// Exit codes 126 and 127 come from nsenter failing to exec nsrestore inside
// the placeholder, so they are named as such with nsenter's own message. A
// context that ended during the run is named whatever the exit looked like:
// on the GPU path cancellation is cooperative and nsrestore exits with a
// status and a reported cause of its own, not a signal.
//
// A failure after nsrestore reported that it was changing the container is
// marked, for containerChanged.
func (o *nsrestoreOutput) failure(runErr, ctxErr error) error {
	err := o.cause(runErr)
	switch {
	case errors.Is(ctxErr, context.DeadlineExceeded):
		err = fmt.Errorf("%w; the restore timeout expired", err)
	case errors.Is(ctxErr, context.Canceled):
		err = fmt.Errorf("%w; the restore was cancelled", err)
	}
	if changingContainer, _ := o.stdoutLines(); changingContainer {
		return &changedContainerError{err: err}
	}
	return err
}

// changedContainerError marks a failure that came after the restore began
// changing the destination container, so the container is no longer the one
// Kubernetes started.
type changedContainerError struct {
	err error
}

func (e *changedContainerError) Error() string { return e.err.Error() }

func (e *changedContainerError) Unwrap() error { return e.err }

// containerChanged reports whether err came after the restore began changing
// the destination container.
func containerChanged(err error) bool {
	var changed *changedContainerError
	return errors.As(err, &changed)
}

func (o *nsrestoreOutput) cause(runErr error) error {
	var result RestoreInNamespaceResult
	if err := json.Unmarshal([]byte(o.resultLine()), &result); err == nil && result.Error != "" {
		return reportedCause(result)
	}
	detail := o.detail()
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		return snapshotruntime.WithDetail(fmt.Errorf("launch nsrestore: %w", runErr), detail)
	}
	switch code := exitErr.ExitCode(); code {
	case -1:
		return snapshotruntime.WithDetail(fmt.Errorf("nsrestore was killed (%s)", exitErr.ProcessState), detail)
	case 126:
		return snapshotruntime.WithDetail(errors.New("nsenter could not start nsrestore inside the placeholder (exit 126, not executable)"), detail)
	case 127:
		return snapshotruntime.WithDetail(errors.New("nsenter could not start nsrestore inside the placeholder (exit 127, executable not found)"), detail)
	default:
		return snapshotruntime.WithDetail(fmt.Errorf("nsrestore exited with status %d without reporting a cause", code), detail)
	}
}

// reportedCause rebuilds the step nsrestore named, if any, so the host side
// reports the same step and does not prefix it again with its own. nsrestore
// renders the step into the error text as well, so that prefix is dropped
// before the step is reattached. A PageBroker code is rebuilt beneath the
// text, which already names it.
func reportedCause(result RestoreInNamespaceResult) error {
	message := result.Error
	if result.Step != "" {
		message = strings.TrimPrefix(message, stepPrefix(Step(result.Step)))
	}
	cause := errors.New(message)
	if code, ok := pagebroker.Failure_Code_value[result.Code]; ok {
		cause = &reportedFailure{message: message, failure: pagebroker.NewFailureError(pagebroker.Failure_Code(code), message)}
	}
	if result.Step == "" {
		return cause
	}
	return failStep(Step(result.Step), cause)
}

// reportedFailure is a PageBroker failure nsrestore reported: its message is
// nsrestore's text, and errors.As still finds the code.
type reportedFailure struct {
	message string
	failure *pagebroker.FailureError
}

func (e *reportedFailure) Error() string { return e.message }

func (e *reportedFailure) Unwrap() error { return e.failure }

// detail prefers a cause line (nsenter, panic) and otherwise the last line
// nsrestore logged at error level or above. nsrestore's stderr is its log, so
// the generic last-line fallback would surface a routine info line or a
// stack frame as if it were the cause; with nothing at error level the
// detail stays empty.
func (o *nsrestoreOutput) detail() string {
	line := o.CauseLine()
	if line == "" {
		line = lastErrorLogLine(o.Stderr())
	}
	_, other := o.stdoutLines()
	return snapshotruntime.JoinDetail(line, strings.Join(other, "\n"))
}

// lastErrorLogLine reads zap's console encoding, which the agent and
// nsrestore log in: tab-separated time, level, logger name, caller, message
// and an optional JSON object of fields. It returns the message of the last
// line at error level or above, with the logged error appended.
func lastErrorLogLine(stderr []byte) string {
	last := ""
	for _, line := range strings.Split(string(stderr), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) < 3 || !isErrorLevel(fields[1]) {
			continue
		}
		msg := fields[len(fields)-1]
		if strings.HasPrefix(msg, "{") && len(fields) >= 4 {
			var logged struct {
				Error string `json:"error"`
			}
			msg = fields[len(fields)-2]
			if json.Unmarshal([]byte(fields[len(fields)-1]), &logged) == nil && logged.Error != "" {
				msg += ": " + logged.Error
			}
		}
		if msg = strings.TrimSpace(msg); msg != "" {
			last = msg
		}
	}
	return last
}

func isErrorLevel(level string) bool {
	switch level {
	case "ERROR", "DPANIC", "PANIC", "FATAL":
		return true
	}
	return false
}

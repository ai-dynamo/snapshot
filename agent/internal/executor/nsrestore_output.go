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

func (o *nsrestoreOutput) result() (*RestoreInNamespaceResult, error) {
	var result RestoreInNamespaceResult
	if err := json.Unmarshal(o.Stdout(), &result); err != nil {
		return nil, fmt.Errorf("failed to parse nsrestore result: %w\nstdout: %s", err, o.Stdout())
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
func (o *nsrestoreOutput) failure(runErr, ctxErr error) error {
	err := o.cause(runErr)
	switch {
	case errors.Is(ctxErr, context.DeadlineExceeded):
		return fmt.Errorf("%w; the restore timeout expired", err)
	case errors.Is(ctxErr, context.Canceled):
		return fmt.Errorf("%w; the restore was cancelled", err)
	}
	return err
}

func (o *nsrestoreOutput) cause(runErr error) error {
	var result RestoreInNamespaceResult
	if err := json.Unmarshal(o.Stdout(), &result); err == nil && result.Error != "" {
		return errors.New(result.Error)
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
	return o.DetailFrom(line)
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

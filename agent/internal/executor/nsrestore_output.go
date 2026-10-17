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
// the placeholder, so they are named as such with nsenter's own message.
func (o *nsrestoreOutput) failure(runErr, ctxErr error) error {
	var result RestoreInNamespaceResult
	if err := json.Unmarshal(o.Stdout(), &result); err == nil && result.Error != "" {
		return errors.New(result.Error)
	}
	detail := o.Detail()
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		return snapshotruntime.WithDetail(fmt.Errorf("launch nsrestore: %w", runErr), detail)
	}
	switch code := exitErr.ExitCode(); code {
	case -1:
		cause := fmt.Sprintf("nsrestore was killed (%s)", exitErr.ProcessState)
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			cause += " after the restore timeout expired"
		}
		return snapshotruntime.WithDetail(errors.New(cause), detail)
	case 126:
		return snapshotruntime.WithDetail(errors.New("nsenter could not start nsrestore inside the placeholder (exit 126, not executable)"), detail)
	case 127:
		return snapshotruntime.WithDetail(errors.New("nsenter could not start nsrestore inside the placeholder (exit 127, executable not found)"), detail)
	default:
		return snapshotruntime.WithDetail(fmt.Errorf("nsrestore exited with status %d without reporting a cause", code), detail)
	}
}

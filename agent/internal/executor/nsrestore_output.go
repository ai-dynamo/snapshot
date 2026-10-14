// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

const (
	nsrestoreStderrTailBytes = 4096
	nsrestoreDetailBytes     = 512
)

// nsrestoreOutput holds what nsrestore wrote while it ran: stdout, which
// carries its JSON result, and a bounded tail of stderr, which is also
// mirrored to the agent's own stderr. A failed run becomes an error that
// names the cause, so restore status and events say what stopped rather
// than a bare exit status.
type nsrestoreOutput struct {
	stdout bytes.Buffer
	stderr tailBuffer
	mirror io.Writer
}

func newNSRestoreOutput(mirror io.Writer) *nsrestoreOutput {
	return &nsrestoreOutput{stderr: tailBuffer{limit: nsrestoreStderrTailBytes}, mirror: mirror}
}

func (o *nsrestoreOutput) attach(cmd *exec.Cmd) {
	cmd.Stdout = &o.stdout
	cmd.Stderr = io.MultiWriter(o.mirror, &o.stderr)
}

func (o *nsrestoreOutput) result() (*RestoreInNamespaceResult, error) {
	var result RestoreInNamespaceResult
	if err := json.Unmarshal(o.stdout.Bytes(), &result); err != nil {
		return nil, fmt.Errorf("failed to parse nsrestore result: %w\nstdout: %s", err, o.stdout.String())
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
	if err := json.Unmarshal(o.stdout.Bytes(), &result); err == nil && result.Error != "" {
		return errors.New(result.Error)
	}
	detail := o.detail()
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		return withDetail(fmt.Errorf("launch nsrestore: %w", runErr), detail)
	}
	switch code := exitErr.ExitCode(); code {
	case -1:
		cause := fmt.Sprintf("nsrestore was killed (%s)", exitErr.ProcessState)
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			cause += " after the restore timeout expired"
		}
		return withDetail(errors.New(cause), detail)
	case 126:
		return withDetail(errors.New("nsenter could not start nsrestore inside the placeholder (exit 126, not executable)"), detail)
	case 127:
		return withDetail(errors.New("nsenter could not start nsrestore inside the placeholder (exit 127, executable not found)"), detail)
	default:
		return withDetail(fmt.Errorf("nsrestore exited with status %d without reporting a cause", code), detail)
	}
}

func (o *nsrestoreOutput) detail() string {
	var parts []string
	if line := stderrSummary(o.stderr.Bytes()); line != "" {
		parts = append(parts, line)
	}
	if out := strings.TrimSpace(o.stdout.String()); out != "" {
		parts = append(parts, "stdout: "+truncateDetail(out))
	}
	return strings.Join(parts, "; ")
}

func withDetail(err error, detail string) error {
	if detail == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, detail)
}

func stderrSummary(stderr []byte) string {
	last := ""
	for _, line := range strings.Split(string(stderr), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "nsenter:") || strings.HasPrefix(line, "panic:") || strings.HasPrefix(line, "fatal error:") {
			return truncateDetail(line)
		}
		last = line
	}
	return truncateDetail(last)
}

func truncateDetail(s string) string {
	if len(s) <= nsrestoreDetailBytes {
		return s
	}
	return s[:nsrestoreDetailBytes] + "..."
}

type tailBuffer struct {
	limit int
	data  []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	if len(b.data) > b.limit {
		b.data = append([]byte(nil), b.data[len(b.data)-b.limit:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) Bytes() []byte { return b.data }

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package logging

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/go-logr/logr"

	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
)

// LogProcessDiagnostics logs process state and CRIU restore log for debugging a failed restore.
func LogProcessDiagnostics(procRoot string, pid int, restoreLogPath string, log logr.Logger) {
	entry := log.WithValues("restored_pid", pid)

	// Process status and cmdline
	pidStr := strconv.Itoa(pid)
	if data, err := os.ReadFile(filepath.Join(procRoot, pidStr, "status")); err == nil {
		entry.Info("Process status", "content", strings.TrimSpace(string(data)))
	}
	if data, err := os.ReadFile(filepath.Join(procRoot, pidStr, "cmdline")); err == nil {
		cmdline := strings.TrimSpace(strings.ReplaceAll(string(data), "\x00", " "))
		if cmdline != "" {
			entry.Info("Process cmdline", "cmdline", cmdline)
		}
	}

	// Exit code from /proc/stat
	if data, err := os.ReadFile(filepath.Join(procRoot, pidStr, "stat")); err == nil {
		if ws, err := snapshotruntime.ParseProcExitCode(string(data)); err == nil {
			entry.Info("Process exit code", "exit_status", ws.ExitStatus(), "term_signal", ws.Signal(), "core_dumped", ws.CoreDump())
		}
	}

	// PID 1 children in restored namespace
	if data, err := os.ReadFile(filepath.Join(procRoot, "1", "task", "1", "children")); err == nil {
		entry.Info("PID 1 children", "children", strings.TrimSpace(string(data)))
	}

	// CRIU restore log summary
	logRestoreLog(restoreLogPath, entry)
}

// LogRestoreErrors finds the CRIU restore.log and logs key lines from it.
func LogRestoreErrors(checkpointPath, workDir string, log logr.Logger) {
	if logPath := findRestoreLog(checkpointPath, workDir); logPath != "" {
		logRestoreLog(logPath, log)
	}
}

// LastRestoreError returns the last error CRIU logged in restore.log, or "" when
// there is none. CRIU's RPC response carries the first error it logged instead,
// which is often an earlier, non-fatal one.
func LastRestoreError(checkpointPath, workDir string) string {
	logPath := findRestoreLog(checkpointPath, workDir)
	if logPath == "" {
		return ""
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		return ""
	}
	return lastCRIUError(string(data))
}

// criuRestoreSummary is the line CRIU logs once restore has already failed.
// Errors logged after it come from teardown, not from what stopped the restore.
const criuRestoreSummary = "Restoring FAILED."

// criuFollowOnError matches errors CRIU logs after the failure that caused
// them: the restore summary and the exit of a task that hit the real error.
var criuFollowOnError = regexp.MustCompile(`^(Restoring FAILED\.|\d+ exited, status=\d+|\d+ killed by signal \d+: .*)$`)

// lastCRIUError returns the message of the last "Error (file:line): msg" line
// logged before the restore summary, skipping follow-on errors unless nothing
// else was logged.
func lastCRIUError(content string) string {
	lines := strings.Split(content, "\n")
	summary := len(lines)
	for i, line := range lines {
		if criuErrorMessage(line) == criuRestoreSummary {
			summary = i
			break
		}
	}
	fallback := ""
	for i := min(summary, len(lines)-1); i >= 0; i-- {
		msg := criuErrorMessage(lines[i])
		if msg == "" || msg == criuRestoreSummary {
			continue
		}
		if !criuFollowOnError.MatchString(msg) {
			return msg
		}
		if fallback == "" {
			fallback = msg
		}
	}
	if fallback != "" {
		return fallback
	}
	for i := len(lines) - 1; i > summary; i-- {
		if msg := criuErrorMessage(lines[i]); msg != "" {
			return msg
		}
	}
	return ""
}

// criuErrorMessage returns the message of an "Error (file:line): msg" line, or
// "" for any other line.
func criuErrorMessage(line string) string {
	_, rest, found := strings.Cut(line, "Error (")
	if !found {
		return ""
	}
	_, msg, found := strings.Cut(rest, "): ")
	if !found {
		return ""
	}
	return strings.TrimSpace(msg)
}

// findRestoreLog returns the CRIU restore.log path, trying the work dir first
// and then the checkpoint dir, or "" when neither has one.
func findRestoreLog(checkpointPath, workDir string) string {
	for _, dir := range []string{workDir, checkpointPath} {
		if dir == "" {
			continue
		}
		logPath := filepath.Join(dir, "restore.log")
		if _, err := os.Stat(logPath); err == nil {
			return logPath
		}
	}
	return ""
}

// logRestoreLog extracts key lines and tail from a CRIU restore log file.
func logRestoreLog(path string, log logr.Logger) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	lines := strings.Split(string(data), "\n")

	// Extract error/warning/notable lines
	var keyLines []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		if strings.Contains(lower, "error") ||
			strings.Contains(lower, "warn") ||
			strings.Contains(lower, "fail") ||
			strings.Contains(lower, "cuda") ||
			strings.Contains(lower, "restore finished successfully") {
			keyLines = append(keyLines, trimmed)
			if len(keyLines) >= 80 {
				break
			}
		}
	}
	if len(keyLines) > 0 {
		log.Info("CRIU restore key lines", "path", path, "lines", strings.Join(keyLines, " | "))
	}

	// Last 40 non-empty lines
	var tail []string
	for i := len(lines) - 1; i >= 0 && len(tail) < 40; i-- {
		if trimmed := strings.TrimSpace(lines[i]); trimmed != "" {
			tail = append(tail, trimmed)
		}
	}
	slices.Reverse(tail)
	if len(tail) > 0 {
		log.Info("CRIU restore tail", "path", path, "lines", strings.Join(tail, " | "))
	}
}

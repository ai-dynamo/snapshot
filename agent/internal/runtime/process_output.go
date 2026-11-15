// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

const (
	processStderrTailBytes = 4096
	processDetailBytes     = 512
)

// ProcessOutput holds what a child process wrote: all of stdout and a
// bounded tail of stderr, with stderr also mirrored to another writer so
// the agent log keeps the full stream. Detail reduces what was kept to one
// line fit for a condition message.
type ProcessOutput struct {
	stdout bytes.Buffer
	stderr stderrTail
	mirror io.Writer
}

// NewProcessOutput mirrors stderr to mirror. The first stderr line starting
// with one of causePrefixes is remembered as it is written, so it survives
// however much the process prints after it, and is preferred as the detail
// over the last line written.
func NewProcessOutput(mirror io.Writer, causePrefixes ...string) ProcessOutput {
	return ProcessOutput{
		stderr: stderrTail{tail: tailBuffer{limit: processStderrTailBytes}, causePrefixes: causePrefixes},
		mirror: mirror,
	}
}

// Attach points cmd's stdout and stderr at this collector.
func (o *ProcessOutput) Attach(cmd *exec.Cmd) {
	cmd.Stdout = &o.stdout
	cmd.Stderr = io.MultiWriter(o.mirror, &o.stderr)
}

// Stdout returns everything the process wrote to stdout.
func (o *ProcessOutput) Stdout() []byte { return o.stdout.Bytes() }

// Stderr returns the kept tail of what the process wrote to stderr.
func (o *ProcessOutput) Stderr() []byte { return o.stderr.Bytes() }

// CauseLine returns the first stderr line that started with a cause prefix,
// or "" when none did.
func (o *ProcessOutput) CauseLine() string { return o.stderr.causeLine() }

// LastStderrLine returns the last non-empty line in the kept stderr tail.
func (o *ProcessOutput) LastStderrLine() string {
	last := ""
	for _, line := range strings.Split(string(o.stderr.Bytes()), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			last = line
		}
	}
	return truncateDetail(last)
}

// Detail is the one line that best explains a failed run: the cause line or
// the last stderr line, then any stdout, each cut to a bounded length.
func (o *ProcessOutput) Detail() string {
	line := o.CauseLine()
	if line == "" {
		line = o.LastStderrLine()
	}
	return o.DetailFrom(line)
}

// DetailFrom builds the detail around a caller-chosen stderr line, for
// callers that know how to read their process's stderr better than the last
// line heuristic does.
func (o *ProcessOutput) DetailFrom(stderrLine string) string {
	var parts []string
	if stderrLine != "" {
		parts = append(parts, truncateDetail(stderrLine))
	}
	if out := strings.TrimSpace(o.stdout.String()); out != "" {
		parts = append(parts, "stdout: "+truncateDetail(out))
	}
	return strings.Join(parts, "; ")
}

// WithDetail appends detail to err's message when there is one.
func WithDetail(err error, detail string) error {
	if detail == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, detail)
}

func truncateDetail(s string) string {
	if len(s) <= processDetailBytes {
		return s
	}
	return s[:processDetailBytes] + "..."
}

type stderrTail struct {
	tail          tailBuffer
	causePrefixes []string
	cause         string
	partial       []byte
}

func (s *stderrTail) Write(p []byte) (int, error) {
	_, _ = s.tail.Write(p)
	if s.cause == "" {
		s.scan(p)
	}
	return len(p), nil
}

func (s *stderrTail) Bytes() []byte { return s.tail.Bytes() }

func (s *stderrTail) causeLine() string {
	if s.cause == "" && s.matches(s.partial) {
		return truncateDetail(strings.TrimSpace(string(s.partial)))
	}
	return s.cause
}

func (s *stderrTail) scan(p []byte) {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.appendPartial(p)
			return
		}
		s.appendPartial(p[:i])
		line, rest := s.partial, p[i+1:]
		s.partial = nil
		if s.matches(line) {
			s.cause = truncateDetail(strings.TrimSpace(string(line)))
			return
		}
		p = rest
	}
}

func (s *stderrTail) appendPartial(p []byte) {
	if room := processDetailBytes + 1 - len(s.partial); room > 0 {
		s.partial = append(s.partial, p[:min(room, len(p))]...)
	}
}

func (s *stderrTail) matches(line []byte) bool {
	trimmed := strings.TrimSpace(string(line))
	for _, prefix := range s.causePrefixes {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
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

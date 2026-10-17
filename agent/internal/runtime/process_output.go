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
	stdout        bytes.Buffer
	stderr        tailBuffer
	mirror        io.Writer
	causePrefixes []string
}

// NewProcessOutput mirrors stderr to mirror. A stderr line starting with one
// of causePrefixes is preferred as the detail over the last line written.
func NewProcessOutput(mirror io.Writer, causePrefixes ...string) ProcessOutput {
	return ProcessOutput{
		stderr:        tailBuffer{limit: processStderrTailBytes},
		mirror:        mirror,
		causePrefixes: causePrefixes,
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

// Detail is the one line that best explains a failed run: the preferred or
// last stderr line, then any stdout, each cut to a bounded length.
func (o *ProcessOutput) Detail() string {
	var parts []string
	if line := o.stderrSummary(); line != "" {
		parts = append(parts, line)
	}
	if out := strings.TrimSpace(o.stdout.String()); out != "" {
		parts = append(parts, "stdout: "+truncateDetail(out))
	}
	return strings.Join(parts, "; ")
}

func (o *ProcessOutput) stderrSummary() string {
	last := ""
	for _, line := range strings.Split(string(o.stderr.Bytes()), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, prefix := range o.causePrefixes {
			if strings.HasPrefix(line, prefix) {
				return truncateDetail(line)
			}
		}
		last = line
	}
	return truncateDetail(last)
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

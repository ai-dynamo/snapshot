// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
)

func processOutputOf(t *testing.T, stdout, stderr string, causePrefixes ...string) *processOutput {
	t.Helper()
	output := newProcessOutput(io.Discard, causePrefixes...)
	output.stdout.WriteString(stdout)
	if _, err := output.stderr.Write([]byte(stderr)); err != nil {
		t.Fatal(err)
	}
	return &output
}

func TestProcessOutputDetail(t *testing.T) {
	long := strings.Repeat("x", processDetailBytes+1)
	tests := []struct {
		name     string
		stdout   string
		stderr   string
		prefixes []string
		want     string
	}{
		{name: "nothing written"},
		{name: "last stderr line", stderr: "first\nsecond\n\n", want: "second"},
		{name: "preferred prefix beats the last line", stderr: "log\nnsenter: no\nmore\n", prefixes: []string{"nsenter:"}, want: "nsenter: no"},
		{name: "first preferred line wins", stderr: "panic: a\npanic: b\n", prefixes: []string{"panic:"}, want: "panic: a"},
		{name: "prefix only matches at line start", stderr: "saw nsenter: x\nlast\n", prefixes: []string{"nsenter:"}, want: "last"},
		{name: "stdout follows stderr", stdout: " out ", stderr: "err\n", want: "err; stdout: out"},
		{name: "stdout alone", stdout: "out", want: "stdout: out"},
		{name: "long lines are cut", stderr: long + "\n", want: long[:processDetailBytes] + "..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := processOutputOf(t, tt.stdout, tt.stderr, tt.prefixes...).detail(); got != tt.want {
				t.Fatalf("detail() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWithDetail(t *testing.T) {
	cause := errors.New("boom")
	if err := withDetail(cause, ""); err != cause {
		t.Fatalf("withDetail(empty) = %v, want the error unchanged", err)
	}
	err := withDetail(cause, "more")
	if !errors.Is(err, cause) || err.Error() != "boom: more" {
		t.Fatalf("withDetail = %v", err)
	}
}

func TestProcessOutputMirrorsStderr(t *testing.T) {
	var mirror bytes.Buffer
	output := newProcessOutput(&mirror)
	cmd := exec.Command("sh", "-c", "echo result; echo to-stderr >&2")
	output.attach(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if got := mirror.String(); got != "to-stderr\n" {
		t.Fatalf("mirror = %q", got)
	}
	if got := string(output.stderr.Bytes()); got != "to-stderr\n" {
		t.Fatalf("stderr tail = %q", got)
	}
	if got := output.stdout.String(); got != "result\n" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestTailBufferKeepsTheEnd(t *testing.T) {
	buf := tailBuffer{limit: 8}
	for _, chunk := range []string{"0123", "4567", "89ab", "cd"} {
		if _, err := buf.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if got := string(buf.Bytes()); got != "6789abcd" {
		t.Fatalf("tail = %q, want %q", got, "6789abcd")
	}
}

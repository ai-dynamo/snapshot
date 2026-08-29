// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
)

// A CUDA workload maps thousands of paths containing "cuda", and CRIU logs one
// informational "Collected [...]" line for each. An unordered scan fills the
// key-line cap with that noise and drops the actual failure, which CRIU logs
// last -- the symptom was a restore whose only reported error was an unrelated
// early kerndat probe.
func TestLogRestoreLogKeepsFailureUnderNoise(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "restore.log")

	var buf strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&buf, "(00.0000) Collected [usr/local/lib/python3.12/cuda_%d.so] ID 0x%x\n", i, i)
	}
	buf.WriteString("(02.5380) Error (criu/cr-restore.c:2390): Restoring FAILED.\n")
	// CRIU keeps unwinding after the failure, so the tail window alone does not
	// preserve it either.
	for i := 0; i < 2*keyLineLimit; i++ {
		fmt.Fprintf(&buf, "(02.5390) Unlink remap /tmp/exported.so.cr.%x.ghost\n", i)
	}

	if err := os.WriteFile(path, []byte(buf.String()), 0o600); err != nil {
		t.Fatalf("write restore log: %v", err)
	}

	var keyLines string
	log := funcr.New(func(_, args string) {
		if strings.Contains(args, "CRIU restore key lines") {
			keyLines = args
		}
	}, funcr.Options{})

	logRestoreLog(path, log)

	if !strings.Contains(keyLines, "Restoring FAILED.") {
		t.Fatalf("failure line must survive the key-line cap, got: %s", keyLines)
	}
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package logging

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLastCRIUError(t *testing.T) {
	tests := []struct {
		name string
		log  string
		want string
	}{
		{
			name: "reports the fatal error, not an earlier one",
			log: `(00.010231) 1: Error (criu/mount.c:2051): binfmt_misc: Operation not permitted
(00.054120) 1: Warn  (criu/mount-v2.c:301): mnt-v2: Unable to set group
(00.082397) 1: Error (criu/mount-v2.c:634): mnt-v2: Can't stat mountpoint /tmp/.criu.mntns.x/mnt-0000011107/cache/vllm: No such file or directory
(00.083100) Error (criu/cr-restore.c:1273): 1 exited, status=1
(00.083487) Error (criu/cr-restore.c:2390): Restoring FAILED.
`,
			want: "mnt-v2: Can't stat mountpoint /tmp/.criu.mntns.x/mnt-0000011107/cache/vllm: No such file or directory",
		},
		{
			name: "falls back to a task exit when nothing else failed",
			log: `(00.083100) Error (criu/cr-restore.c:1277): 7 killed by signal 9: Killed
(00.083487) Error (criu/cr-restore.c:2390): Restoring FAILED.
`,
			want: "7 killed by signal 9: Killed",
		},
		{
			name: "summary alone is not a cause",
			log:  "(00.083487) Error (criu/cr-restore.c:2390): Restoring FAILED.\n",
		},
		{
			name: "no errors",
			log:  "(00.000001) Restore finished successfully. Tasks resumed.\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := lastCRIUError(tc.log); got != tc.want {
				t.Errorf("lastCRIUError() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLastRestoreErrorPrefersWorkDir(t *testing.T) {
	workDir := t.TempDir()
	checkpointDir := t.TempDir()
	writeRestoreLog(t, workDir, "(00.1) 1: Error (criu/files.c:10): work dir error\n")
	writeRestoreLog(t, checkpointDir, "(00.1) 1: Error (criu/files.c:10): checkpoint dir error\n")

	if got := LastRestoreError(checkpointDir, workDir); got != "work dir error" {
		t.Errorf("LastRestoreError() = %q, want work dir error", got)
	}
	if got := LastRestoreError(checkpointDir, ""); got != "checkpoint dir error" {
		t.Errorf("LastRestoreError() without work dir = %q, want checkpoint dir error", got)
	}
	if got := LastRestoreError(t.TempDir(), ""); got != "" {
		t.Errorf("LastRestoreError() without restore.log = %q, want empty", got)
	}
}

func writeRestoreLog(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "restore.log"), []byte(content), 0o600); err != nil {
		t.Fatalf("write restore.log: %v", err)
	}
}

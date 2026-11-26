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
			name: "ignores teardown errors logged after the summary",
			log: `(00.004280) Error (criu/tun.c:85): tun: Unable to create tun: No such file or directory
(00.035584)      1: Error (criu/mount-v2.c:634): mnt-v2: Can't stat mountpoint /tmp/.criu.mntns.31gkOS/mnt-0000001303/cache/volume-9: No such file or directory
(00.035998) Error (criu/cr-restore.c:2419): Restoring FAILED.
(00.036618) Error (criu/cgroup.c:2247): cg: cgroupd: recv req error: No such file or directory
`,
			want: "mnt-v2: Can't stat mountpoint /tmp/.criu.mntns.31gkOS/mnt-0000001303/cache/volume-9: No such file or directory",
		},
		{
			name: "reports a teardown error when nothing before the summary failed",
			log: `(00.035998) Error (criu/cr-restore.c:2419): Restoring FAILED.
(00.036618) Error (criu/cgroup.c:2247): cg: cgroupd: recv req error: No such file or directory
`,
			want: "cg: cgroupd: recv req error: No such file or directory",
		},
		{
			name: "falls back to a task exit when nothing else failed",
			log: `(00.083100) Error (criu/cr-restore.c:1277): 7 killed by signal 9: Killed
(00.083487) Error (criu/cr-restore.c:2390): Restoring FAILED.
`,
			want: "7 killed by signal 9: Killed",
		},
		{
			name: "names the file, not the descriptor CRIU could not reopen",
			log: `(00.003879) Error (criu/tun.c:85): tun: Unable to create tun: No such file or directory
(00.044215)      1: Error (criu/mount.c:2274): mnt: Unable to remove /tmp/cr-tmpfs.q2iQ0G: Device or resource busy
(00.063008)      1: Error (criu/files-reg.c:2175): File etc/legal has bad size 267 (expect 291)
(00.063017)      1: Error (criu/files.c:1235): Unable to open fd=3 id=0x8
(00.063764)      1: async: daemon stopped
(00.064668) mnt: Switching to new ns to clean ghosts
(00.064871) Error (criu/cr-restore.c:2419): Restoring FAILED.
(00.065831) Error (criu/cgroup.c:2247): cg: cgroupd: recv req error: No such file or directory
`,
			want: "File etc/legal has bad size 267 (expect 291)",
		},
		{
			name: "names why a kernel probe failed, not only which one",
			log: `(00.003948) Error (criu/tun.c:85): tun: Unable to create tun: No such file or directory
(00.023802) Error (criu/kerndat.c:874): Can't create inotify: Too many open files
(00.023806) Error (criu/kerndat.c:2162): kerndat_has_inotify_setnextwd failed when initializing kerndat.
`,
			want: "Can't create inotify: Too many open files",
		},
		{
			name: "keeps a wrapper error with no error on the line before it",
			log: `(00.003879) Error (criu/tun.c:85): tun: Unable to create tun: No such file or directory
(00.063001)      1: Restoring fd 3
(00.063017)      1: Error (criu/files.c:1235): Unable to open fd=3 id=0x8
(00.064871) Error (criu/cr-restore.c:2419): Restoring FAILED.
`,
			want: "Unable to open fd=3 id=0x8",
		},
		{
			name: "keeps a wrapper error when another task logged the line before it",
			log: `(00.063008)     37: Error (criu/files-reg.c:2175): File etc/legal has bad size 267 (expect 291)
(00.063017)      1: Error (criu/files.c:1235): Unable to open fd=3 id=0x8
(00.064871) Error (criu/cr-restore.c:2419): Restoring FAILED.
`,
			want: "Unable to open fd=3 id=0x8",
		},
		{
			name: "names the cause in a log without timestamps",
			log: `Error (criu/tun.c:85): tun: Unable to create tun: No such file or directory
     1: Error (criu/files-reg.c:2175): File etc/legal has bad size 267 (expect 291)
     1: Error (criu/files.c:1235): Unable to open fd=3 id=0x8
Error (criu/cr-restore.c:2419): Restoring FAILED.
`,
			want: "File etc/legal has bad size 267 (expect 291)",
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

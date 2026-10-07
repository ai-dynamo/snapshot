// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// SetProcessGroupCancellation makes cancellation kill a helper and its children,
// including children that keep the command's output pipes open. After the kill, Wait
// returns once waitDelay passes even if descendants still hold those pipes.
func SetProcessGroupCancellation(cmd *exec.Cmd, waitDelay time.Duration) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return normalizeProcessGroupKillError(syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL))
	}
	cmd.WaitDelay = waitDelay
}

func normalizeProcessGroupKillError(err error) error {
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

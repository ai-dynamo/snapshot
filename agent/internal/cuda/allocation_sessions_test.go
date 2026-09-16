// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"os"
	"os/exec"
	"slices"
	"testing"
)

func TestAllocationSessionsAppendAndClose(t *testing.T) {
	session, err := os.Open("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	existing, err := os.Open("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	defer existing.Close()
	id := "0123456789abcdef0123456789abcdef"
	sessions := AllocationSessions{id: session}
	cmd := exec.Command("coordinator", "--prepare")
	// Capture reserves eight descriptors, so the first capability must be fd11.
	cmd.ExtraFiles = make([]*os.File, 8)
	cmd.ExtraFiles[0] = existing
	sessions.AppendTo(cmd)
	want := []string{"coordinator", "--prepare", "--content-storage", "pagebroker", "--allocation-session", id, "11"}
	if !slices.Equal(cmd.Args, want) || len(cmd.ExtraFiles) != 9 || cmd.ExtraFiles[0] != existing || cmd.ExtraFiles[8] != session {
		t.Fatalf("unexpected capability mapping: %v, files=%v", cmd.Args, cmd.ExtraFiles)
	}
	sessions.Close()
	sessions.Close()
	if len(sessions) != 0 {
		t.Fatal("capability map not drained")
	}
	if _, err := session.Stat(); err == nil {
		t.Fatal("session descriptor remained open")
	}
	if _, err := existing.Stat(); err != nil {
		t.Fatalf("namespace descriptor was closed: %v", err)
	}
}

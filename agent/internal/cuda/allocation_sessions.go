// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
)

// AllocationSessions owns already-bound broker connections, keyed by durable
// shim identity rather than PID or enumeration order.
type AllocationSessions map[string]*os.File

func (sessions AllocationSessions) Close() {
	for id, file := range sessions {
		_ = file.Close()
		delete(sessions, id)
	}
}

// AppendTo adds capabilities after the command's existing namespace descriptors.
// Only the coordinator receives them, never the workload's general environment.
func (sessions AllocationSessions) AppendTo(cmd *exec.Cmd) {
	if sessions == nil {
		return
	}
	cmd.Args = append(cmd.Args, "--content-storage", "pagebroker")
	for id, file := range sessions {
		cmd.Args = append(cmd.Args, "--allocation-session", id, strconv.Itoa(3+len(cmd.ExtraFiles)))
		cmd.ExtraFiles = append(cmd.ExtraFiles, file)
	}
}

// BindAllocationSessions preflights every participant, including empty creators,
// while still in the trusted agent context. Failure closes all prior admissions.
func BindAllocationSessions(ctx context.Context, broker pagebroker.Client, transaction string, ids []string, direction pagebroker.BindAllocationSession_Direction) (AllocationSessions, error) {
	sessions := make(AllocationSessions, len(ids))
	for _, id := range ids {
		decoded, err := hex.DecodeString(id)
		if err != nil || len(decoded) != 16 || sessions[id] != nil {
			sessions.Close()
			return nil, fmt.Errorf("invalid or duplicate cuinterpose participant %q", id)
		}
		file, err := broker.BindAllocations(ctx, transaction, id, direction)
		if err != nil {
			sessions.Close()
			return nil, fmt.Errorf("bind allocation participant %s: %w", id, err)
		}
		sessions[id] = file
	}
	if len(sessions) == 0 {
		return nil, fmt.Errorf("allocation storage requires CUDA participants")
	}
	return sessions, nil
}

// CapturedParticipants validates the coordinator state before CRIU or GPU writes.
func CapturedParticipants(ctx context.Context, checkpoint string, pids []int) ([]string, error) {
	args, err := cuinterposeArgs("state-participants", checkpoint, "", "/snapshot-control", pids, pids)
	if err != nil {
		return nil, err
	}
	output, err := exec.CommandContext(ctx, DefaultCoordinatorBinaryPath, args...).Output()
	if err != nil {
		return nil, fmt.Errorf("read captured cuinterpose participants: %w", err)
	}
	var ids []string
	if err := json.Unmarshal(output, &ids); err != nil {
		return nil, err
	}
	if len(ids) != len(pids) {
		return nil, fmt.Errorf("captured cuinterpose participant/PID count differs")
	}
	entries, err := os.ReadDir(filepath.Join(checkpoint, "allocations"))
	if err != nil {
		return nil, fmt.Errorf("read allocation participants: %w", err)
	}
	expected := make(map[string]bool, len(ids))
	for _, id := range ids {
		expected[id] = true
	}
	if len(entries) != len(expected) {
		return nil, fmt.Errorf("allocation manifests do not cover exactly the captured participants")
	}
	for _, entry := range entries {
		if !entry.IsDir() || !expected[entry.Name()] {
			return nil, fmt.Errorf("unexpected allocation participant %q", entry.Name())
		}
	}
	return ids, nil
}

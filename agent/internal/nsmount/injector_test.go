// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nsmount

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/go-logr/logr"
)

type fakeMountRef struct {
	dst        string
	unmountLog *[]string
}

func (h *fakeMountRef) NsFd() *os.File { return nil }
func (h *fakeMountRef) Unmount() error {
	*h.unmountLog = append(*h.unmountLog, h.dst)
	return nil
}

type mountCall struct {
	pid      int
	src, dst string
	opts     MountOptions
}

type mockMounter struct {
	results    []error
	calls      []mountCall
	unmountLog []string
}

func (m *mockMounter) Mount(_ context.Context, pid int, src, dst string, opts MountOptions) (mountRef, error) {
	i := len(m.calls)
	m.calls = append(m.calls, mountCall{pid: pid, src: src, dst: dst, opts: opts})
	if i < len(m.results) && m.results[i] != nil {
		return nil, m.results[i]
	}
	return &fakeMountRef{dst: dst, unmountLog: &m.unmountLog}, nil
}

const testPID = 42

func newMounter(t *testing.T, m *mockMounter) *NSMounter {
	t.Helper()
	return newWithMounter(m, logr.Discard())
}

func TestMountUsesCallerPathsAndReadOnlyPolicy(t *testing.T) {
	m := &mockMounter{}
	nsm := newMounter(t, m)

	for _, tc := range []struct {
		mounter  *NSMounter
		src, dst string
	}{
		{nsm, SnapshotBinSrc, SnapshotBinDst},
		{nsm.WithNoExec(), "/checkpoints/artifacts/content-uid/containers/main", CheckpointDst},
	} {
		if _, err := tc.mounter.Mount(context.Background(), testPID, tc.src, tc.dst); err != nil {
			t.Fatalf("Mount(%s, %s): %v", tc.src, tc.dst, err)
		}
	}

	want := []mountCall{
		{pid: testPID, src: SnapshotBinSrc, dst: SnapshotBinDst, opts: MountOptions{ReadOnly: true}},
		{pid: testPID, src: "/checkpoints/artifacts/content-uid/containers/main", dst: CheckpointDst, opts: MountOptions{ReadOnly: true, NoExec: true}},
	}
	if len(m.calls) != len(want) {
		t.Fatalf("got %d calls, want %d", len(m.calls), len(want))
	}
	for i := range want {
		if m.calls[i] != want[i] {
			t.Errorf("call[%d] = %+v, want %+v", i, m.calls[i], want[i])
		}
	}
}

func TestMountPointUnmount(t *testing.T) {
	m := &mockMounter{}
	mp, err := newMounter(t, m).Mount(context.Background(), testPID, SnapshotBinSrc, SnapshotBinDst)
	if err != nil {
		t.Fatal(err)
	}

	if err := mp.Unmount(); err != nil {
		t.Fatal(err)
	}
	if len(m.unmountLog) != 1 || m.unmountLog[0] != SnapshotBinDst {
		t.Fatalf("unmount log = %v", m.unmountLog)
	}
}

func TestMountFailure(t *testing.T) {
	wantErr := errors.New("mount failed")
	m := &mockMounter{results: []error{wantErr}}
	_, err := newMounter(t, m).Mount(context.Background(), testPID, SnapshotBinSrc, SnapshotBinDst)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Mount() error = %v, want %v", err, wantErr)
	}
}

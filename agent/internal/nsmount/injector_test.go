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
func (h *fakeMountRef) Unmount(context.Context) error {
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

func TestRoleMountsUseFixedPathsAndPolicies(t *testing.T) {
	m := &mockMounter{}
	nsm := newMounter(t, m)

	if _, err := nsm.MountBundle(context.Background(), testPID); err != nil {
		t.Fatalf("MountBundle: %v", err)
	}
	if _, err := nsm.MountArtifact(context.Background(), testPID, "/checkpoints/abc/versions/1"); err != nil {
		t.Fatalf("MountArtifact: %v", err)
	}

	want := []mountCall{
		{pid: testPID, src: SnapshotBinSrc, dst: SnapshotBinDst, opts: MountOptions{ReadOnly: true}},
		{pid: testPID, src: "/checkpoints/abc/versions/1", dst: CheckpointDst, opts: MountOptions{ReadOnly: true, NoExec: true}},
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

func TestMountArtifactRejectsUnsafeSourceBeforeHelper(t *testing.T) {
	for _, source := range []string{
		"/etc",
		"/proc",
		"/checkpoints-other/abc",
		"/checkpoints/../etc",
		"/checkpoints/abc;id",
		"/checkpoints/abc id",
		"/checkpoints/é",
	} {
		t.Run(source, func(t *testing.T) {
			m := &mockMounter{}
			if _, err := newMounter(t, m).MountArtifact(context.Background(), testPID, source); err == nil {
				t.Fatalf("MountArtifact(%q) succeeded", source)
			}
			if len(m.calls) != 0 {
				t.Fatalf("helper called for invalid source %q", source)
			}
		})
	}
}

func TestMountPointUnmount(t *testing.T) {
	m := &mockMounter{}
	mp, err := newMounter(t, m).MountBundle(context.Background(), testPID)
	if err != nil {
		t.Fatal(err)
	}

	if err := mp.Unmount(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.unmountLog) != 1 || m.unmountLog[0] != SnapshotBinDst {
		t.Fatalf("unmount log = %v", m.unmountLog)
	}
}

func TestMountFailure(t *testing.T) {
	wantErr := errors.New("mount failed")
	m := &mockMounter{results: []error{wantErr}}
	_, err := newMounter(t, m).MountBundle(context.Background(), testPID)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Mount() error = %v, want %v", err, wantErr)
	}
}

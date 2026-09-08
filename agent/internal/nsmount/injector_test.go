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
	nsFd       *os.File
	unmountLog *[]string
}

func (h *fakeMountRef) NsFd() *os.File { return h.nsFd }
func (h *fakeMountRef) Unmount(context.Context) error {
	*h.unmountLog = append(*h.unmountLog, h.dst)
	return nil
}

type mountCall struct {
	role string
	pid  int
	nsFd *os.File
	src  string
}

type mockMounter struct {
	results    []error
	calls      []mountCall
	bundleNsFd *os.File
	unmountLog []string
}

func (m *mockMounter) mount(role string, pid int, src string) (mountRef, error) {
	i := len(m.calls)
	m.calls = append(m.calls, mountCall{role: role, pid: pid, src: src})
	if i < len(m.results) && m.results[i] != nil {
		return nil, m.results[i]
	}
	return &fakeMountRef{dst: role, nsFd: m.bundleNsFd, unmountLog: &m.unmountLog}, nil
}

func (m *mockMounter) MountBundle(_ context.Context, pid int) (mountRef, error) {
	return m.mount("bundle", pid, "")
}

func (m *mockMounter) MountPageBroker(_ context.Context, nsFd *os.File, src string) (mountRef, error) {
	i := len(m.calls)
	m.calls = append(m.calls, mountCall{role: "pagebroker", nsFd: nsFd, src: src})
	if i < len(m.results) && m.results[i] != nil {
		return nil, m.results[i]
	}
	return &fakeMountRef{dst: "pagebroker", unmountLog: &m.unmountLog}, nil
}

const testPID = 42

func newMounter(t *testing.T, m *mockMounter) *NSMounter {
	t.Helper()
	return newWithMounter(m, logr.Discard())
}

func TestRoleMountsUseFixedPathsAndPolicies(t *testing.T) {
	namespaceFd, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := namespaceFd.Close(); err != nil {
			t.Errorf("close namespace fd: %v", err)
		}
	})
	m := &mockMounter{bundleNsFd: namespaceFd}
	nsm := newMounter(t, m)

	bundle, err := nsm.MountBundle(context.Background(), testPID)
	if err != nil {
		t.Fatalf("MountBundle: %v", err)
	}
	if _, err := nsm.MountPageBroker(context.Background(), bundle, "/pagebroker/staging/restore/tx-1"); err != nil {
		t.Fatalf("MountPageBroker: %v", err)
	}

	want := []mountCall{
		{role: "bundle", pid: testPID},
		{role: "pagebroker", src: "/pagebroker/staging/restore/tx-1"},
	}
	if len(m.calls) != len(want) {
		t.Fatalf("got %d calls, want %d", len(m.calls), len(want))
	}
	for i := range want {
		if m.calls[i].role != want[i].role || m.calls[i].pid != want[i].pid || m.calls[i].src != want[i].src {
			t.Errorf("call[%d] = %+v, want %+v", i, m.calls[i], want[i])
		}
	}
	if m.calls[1].nsFd != bundle.NsFd() {
		t.Fatal("PageBroker mount did not reuse the bundle's pinned namespace fd")
	}
}

func TestMountPageBrokerRejectsUnsafeSourceBeforeHelper(t *testing.T) {
	for _, source := range []string{
		"/etc",
		"/proc",
		"/checkpoints/artifacts/content-uid/containers/main",
		"/pagebroker/staging/checkpoint/tx",
		"/pagebroker/staging/restore-other/tx",
		"/pagebroker/staging/restore/../etc",
		"/pagebroker/staging/restore/tx;id",
		"/pagebroker/staging/restore/tx id",
		"/pagebroker/staging/restore/é",
	} {
		t.Run(source, func(t *testing.T) {
			m := &mockMounter{}
			if _, err := newMounter(t, m).MountPageBroker(context.Background(), noopNamespaceMount{}, source); err == nil {
				t.Fatalf("MountPageBroker(%q) succeeded", source)
			}
			if len(m.calls) != 0 {
				t.Fatalf("helper called for invalid source %q", source)
			}
		})
	}
}

type noopNamespaceMount struct{}

func (noopNamespaceMount) Unmount(context.Context) error { return nil }
func (noopNamespaceMount) NsFd() *os.File                { return nil }

func TestMountPointUnmount(t *testing.T) {
	m := &mockMounter{}
	mp, err := newMounter(t, m).MountBundle(context.Background(), testPID)
	if err != nil {
		t.Fatal(err)
	}

	if err := mp.Unmount(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.unmountLog) != 1 || m.unmountLog[0] != "bundle" {
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

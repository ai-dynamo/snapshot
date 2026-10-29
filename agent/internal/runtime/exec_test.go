// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	internalapi "k8s.io/cri-api/pkg/apis"
	critesting "k8s.io/cri-api/pkg/apis/testing"
)

type execRecordingService struct {
	*critesting.FakeRuntimeService
	calls   int
	id      string
	cmd     []string
	timeout time.Duration
}

func (s *execRecordingService) ExecSync(_ context.Context, id string, cmd []string, timeout time.Duration) ([]byte, []byte, error) {
	s.calls++
	s.id, s.cmd, s.timeout = id, cmd, timeout
	return []byte("out"), []byte("err"), nil
}

func TestExecSyncRunsInTheContainerWithoutItsScheme(t *testing.T) {
	tests := []struct {
		name        string
		withRuntime func(internalapi.RuntimeService) Runtime
		containerID string
	}{
		{"containerd", func(s internalapi.RuntimeService) Runtime { return &ContainerdRuntime{cri: s} }, "containerd://container-id"},
		{"CRI-O", func(s internalapi.RuntimeService) Runtime { return &CRIORuntime{svc: s} }, "cri-o://container-id"},
		{"bare ID", func(s internalapi.RuntimeService) Runtime { return &ContainerdRuntime{cri: s} }, "container-id"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &execRecordingService{FakeRuntimeService: critesting.NewFakeRuntimeService()}
			stdout, stderr, err := test.withRuntime(service).ExecSync(context.Background(), test.containerID, []string{"/bin/sh", "-c", "true"}, 3*time.Second)
			require.NoError(t, err)
			require.Equal(t, "container-id", service.id)
			require.Equal(t, []string{"/bin/sh", "-c", "true"}, service.cmd)
			require.Equal(t, 3*time.Second, service.timeout)
			require.Equal(t, "out", string(stdout))
			require.Equal(t, "err", string(stderr))
		})
	}
}

func TestExecSyncRefusesAnotherRuntimesContainerID(t *testing.T) {
	service := &execRecordingService{FakeRuntimeService: critesting.NewFakeRuntimeService()}
	_, _, err := (&ContainerdRuntime{cri: service}).ExecSync(context.Background(), "cri-o://container-id", []string{"true"}, time.Second)
	require.ErrorContains(t, err, "mismatched runtime scheme")
	require.Zero(t, service.calls)
}

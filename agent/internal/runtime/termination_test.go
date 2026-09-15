// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	internalapi "k8s.io/cri-api/pkg/apis"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	critesting "k8s.io/cri-api/pkg/apis/testing"
)

type recordingRuntimeService struct {
	*critesting.FakeRuntimeService
	stopCalls   int
	stopID      string
	stopTimeout int64
	stopErr     error
}

func (s *recordingRuntimeService) StopContainer(ctx context.Context, id string, timeout int64) error {
	s.stopCalls++
	s.stopID = id
	s.stopTimeout = timeout
	if s.stopErr != nil {
		return s.stopErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.FakeRuntimeService.StopContainer(ctx, id, timeout)
}

func newRecordingRuntimeService() (*recordingRuntimeService, *critesting.FakeContainer) {
	service := &recordingRuntimeService{FakeRuntimeService: critesting.NewFakeRuntimeService()}
	container := &critesting.FakeContainer{ContainerStatus: runtimeapi.ContainerStatus{
		Id:    "container-id",
		State: runtimeapi.ContainerState_CONTAINER_RUNNING,
	}}
	service.SetFakeContainers([]*critesting.FakeContainer{container})
	return service, container
}

func TestTerminateContainerUsesMatchingRuntimeIdentity(t *testing.T) {
	tests := []struct {
		name        string
		withRuntime func(internalapi.RuntimeService) Runtime
		containerID string
	}{
		{
			name: "containerd scheme",
			withRuntime: func(service internalapi.RuntimeService) Runtime {
				return &ContainerdRuntime{cri: service}
			},
			containerID: "containerd://container-id",
		},
		{
			name: "containerd bare ID",
			withRuntime: func(service internalapi.RuntimeService) Runtime {
				return &ContainerdRuntime{cri: service}
			},
			containerID: "container-id",
		},
		{
			name: "CRI-O kubelet scheme",
			withRuntime: func(service internalapi.RuntimeService) Runtime {
				return &CRIORuntime{svc: service}
			},
			containerID: "cri-o://container-id",
		},
		{
			name: "CRI-O compact scheme",
			withRuntime: func(service internalapi.RuntimeService) Runtime {
				return &CRIORuntime{svc: service}
			},
			containerID: "crio://container-id",
		},
		{
			name: "CRI-O bare ID",
			withRuntime: func(service internalapi.RuntimeService) Runtime {
				return &CRIORuntime{svc: service}
			},
			containerID: "container-id",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, container := newRecordingRuntimeService()

			err := test.withRuntime(service).TerminateContainer(context.Background(), test.containerID)

			require.NoError(t, err)
			require.Equal(t, 1, service.stopCalls)
			require.Equal(t, "container-id", service.stopID)
			require.Zero(t, service.stopTimeout)
			require.Equal(t, runtimeapi.ContainerState_CONTAINER_EXITED, container.State)
		})
	}
}

func TestTerminateContainerRejectsInvalidRuntimeIdentity(t *testing.T) {
	tests := []struct {
		name        string
		withRuntime func(internalapi.RuntimeService) Runtime
		containerID string
	}{
		{
			name: "containerd rejects CRI-O scheme",
			withRuntime: func(service internalapi.RuntimeService) Runtime {
				return &ContainerdRuntime{cri: service}
			},
			containerID: "cri-o://container-id",
		},
		{
			name: "CRI-O rejects containerd scheme",
			withRuntime: func(service internalapi.RuntimeService) Runtime {
				return &CRIORuntime{svc: service}
			},
			containerID: "containerd://container-id",
		},
		{
			name: "empty ID",
			withRuntime: func(service internalapi.RuntimeService) Runtime {
				return &ContainerdRuntime{cri: service}
			},
		},
		{
			name: "empty ID after scheme",
			withRuntime: func(service internalapi.RuntimeService) Runtime {
				return &CRIORuntime{svc: service}
			},
			containerID: "cri-o://",
		},
		{
			name: "unknown scheme",
			withRuntime: func(service internalapi.RuntimeService) Runtime {
				return &ContainerdRuntime{cri: service}
			},
			containerID: "docker://container-id",
		},
		{
			name: "nested scheme",
			withRuntime: func(service internalapi.RuntimeService) Runtime {
				return &ContainerdRuntime{cri: service}
			},
			containerID: "containerd://cri-o://container-id",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, container := newRecordingRuntimeService()

			err := test.withRuntime(service).TerminateContainer(context.Background(), test.containerID)

			require.Error(t, err)
			require.Zero(t, service.stopCalls)
			require.Equal(t, runtimeapi.ContainerState_CONTAINER_RUNNING, container.State)
		})
	}
}

func TestTerminateContainerPreservesContextAndBackendErrors(t *testing.T) {
	t.Run("canceled context", func(t *testing.T) {
		service, container := newRecordingRuntimeService()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := (&ContainerdRuntime{cri: service}).TerminateContainer(ctx, "containerd://container-id")

		require.ErrorIs(t, err, context.Canceled)
		require.Equal(t, 1, service.stopCalls)
		require.Equal(t, runtimeapi.ContainerState_CONTAINER_RUNNING, container.State)
	})

	t.Run("backend error", func(t *testing.T) {
		service, container := newRecordingRuntimeService()
		backendErr := errors.New("runtime unavailable")
		service.stopErr = backendErr

		err := (&CRIORuntime{svc: service}).TerminateContainer(context.Background(), "cri-o://container-id")

		require.ErrorIs(t, err, backendErr)
		require.ErrorContains(t, err, "cri-o://container-id")
		require.Equal(t, 1, service.stopCalls)
		require.Equal(t, runtimeapi.ContainerState_CONTAINER_RUNNING, container.State)
	})
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func TestContainerRunning(t *testing.T) {
	inState := func(state runtimeapi.ContainerState) *fakeContainerStatusService {
		return &fakeContainerStatusService{response: &runtimeapi.ContainerStatusResponse{
			Status: &runtimeapi.ContainerStatus{State: state},
		}}
	}
	for name, tc := range map[string]struct {
		service *fakeContainerStatusService
		want    bool
		wantErr bool
	}{
		"running":         {service: inState(runtimeapi.ContainerState_CONTAINER_RUNNING), want: true},
		"exited":          {service: inState(runtimeapi.ContainerState_CONTAINER_EXITED)},
		"created":         {service: inState(runtimeapi.ContainerState_CONTAINER_CREATED)},
		"unknown":         {service: inState(runtimeapi.ContainerState_CONTAINER_UNKNOWN)},
		"removed":         {service: &fakeContainerStatusService{err: status.Error(codes.NotFound, "no such container")}},
		"no status":       {service: &fakeContainerStatusService{response: &runtimeapi.ContainerStatusResponse{}}, wantErr: true},
		"a runtime error": {service: &fakeContainerStatusService{err: errors.New("connection refused")}, wantErr: true},
		"unavailable":     {service: &fakeContainerStatusService{err: status.Error(codes.Unavailable, "restarting")}, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := containerRunning(context.Background(), tc.service, "ctr")

			if tc.wantErr {
				require.Error(t, err)
				assert.False(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.False(t, tc.service.verbose, "the state needs no verbose status")
		})
	}
}

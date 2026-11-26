// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"context"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// containerRunning reads the container's state from CRI. A container the
// runtime no longer knows is not running.
func containerRunning(ctx context.Context, svc containerStatusService, containerID string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, criCallTimeout)
	defer cancel()

	response, err := svc.ContainerStatus(ctx, containerID, false)
	if status.Code(err) == codes.NotFound {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to get container status for %s: %w", containerID, err)
	}
	if response.GetStatus() == nil {
		return false, fmt.Errorf("container status for %s is missing", containerID)
	}
	return response.GetStatus().GetState() == runtimeapi.ContainerState_CONTAINER_RUNNING, nil
}

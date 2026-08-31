// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"context"
	"fmt"
	"time"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

const containerImageIDTimeout = 10 * time.Second

type containerStatusService interface {
	ContainerStatus(context.Context, string, bool) (*runtimeapi.ContainerStatusResponse, error)
}

func resolveContainerImageID(ctx context.Context, svc containerStatusService, containerID string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, containerImageIDTimeout)
	defer cancel()

	response, err := svc.ContainerStatus(ctx, containerID, false)
	if err != nil {
		return "", fmt.Errorf("failed to get container status for %s: %w", containerID, err)
	}
	imageID := response.GetStatus().GetImageId()
	if imageID == "" {
		return "", fmt.Errorf("container status for %s has no image ID", containerID)
	}
	return imageID, nil
}

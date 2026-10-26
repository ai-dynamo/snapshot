// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"context"
	"fmt"
	"time"
)

// kubeletPodUIDLabel is set by kubelet on every container it creates. It
// names the Pod's UID on the node, which in a vCluster differs from the UID
// of the Pod the vCluster's API shows.
const kubeletPodUIDLabel = "io.kubernetes.pod.uid"

const containerPodUIDTimeout = 10 * time.Second

func resolveContainerPodUID(ctx context.Context, svc containerStatusService, containerID string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, containerPodUIDTimeout)
	defer cancel()

	response, err := svc.ContainerStatus(ctx, containerID, false)
	if err != nil {
		return "", fmt.Errorf("failed to get container status for %s: %w", containerID, err)
	}
	uid := response.GetStatus().GetLabels()[kubeletPodUIDLabel]
	if uid == "" {
		return "", fmt.Errorf("container %s has no %s label", containerID, kubeletPodUIDLabel)
	}
	return uid, nil
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func TestResolveContainerPodUID(t *testing.T) {
	withLabels := func(labels map[string]string) *fakeContainerStatusService {
		return &fakeContainerStatusService{response: &runtimeapi.ContainerStatusResponse{
			Status: &runtimeapi.ContainerStatus{Labels: labels},
		}}
	}

	t.Run("the label kubelet sets", func(t *testing.T) {
		got, err := resolveContainerPodUID(context.Background(),
			withLabels(map[string]string{kubeletPodUIDLabel: "host-uid"}), "ctr")

		require.NoError(t, err)
		assert.Equal(t, "host-uid", got)
	})

	t.Run("no label", func(t *testing.T) {
		_, err := resolveContainerPodUID(context.Background(), withLabels(nil), "ctr")

		require.Error(t, err)
	})

	t.Run("no status", func(t *testing.T) {
		_, err := resolveContainerPodUID(context.Background(),
			&fakeContainerStatusService{response: &runtimeapi.ContainerStatusResponse{}}, "ctr")

		require.Error(t, err)
	})

	t.Run("runtime error", func(t *testing.T) {
		_, err := resolveContainerPodUID(context.Background(),
			&fakeContainerStatusService{err: errors.New("not found")}, "ctr")

		require.Error(t, err)
	})
}

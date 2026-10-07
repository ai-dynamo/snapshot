// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestCuInterposeContainerConfigurationValidate(t *testing.T) {
	for _, policy := range []corev1.PullPolicy{"", corev1.PullAlways, corev1.PullIfNotPresent, corev1.PullNever} {
		require.NoError(t, CuInterposeContainerConfiguration{Image: "registry.example/agent:v1", PullPolicy: policy}.Validate())
	}
	require.ErrorContains(t, CuInterposeContainerConfiguration{Image: " "}.Validate(), "image is required")
	require.ErrorContains(t, CuInterposeContainerConfiguration{Image: "registry.example/agent:v1", PullPolicy: "sometimes"}.Validate(), "pull policy")
}

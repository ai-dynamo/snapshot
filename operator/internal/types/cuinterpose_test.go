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
		require.NoError(t, CuInterposeContainerConfiguration{
			Preparation: MultiGPUPreparationInitContainer, Image: "registry.example/agent:v1", PullPolicy: policy,
		}.Validate())
	}
	// Agent mode adds no init container, so it needs no image.
	require.NoError(t, CuInterposeContainerConfiguration{Preparation: MultiGPUPreparationAgent}.Validate())

	require.ErrorContains(t, CuInterposeContainerConfiguration{Preparation: MultiGPUPreparationInitContainer, Image: " "}.Validate(), "image is required")
	require.ErrorContains(t, CuInterposeContainerConfiguration{
		Preparation: MultiGPUPreparationInitContainer, Image: "registry.example/agent:v1", PullPolicy: "sometimes",
	}.Validate(), "pull policy")
	require.ErrorContains(t, CuInterposeContainerConfiguration{Preparation: "nri"}.Validate(), "unsupported multi-GPU preparation")
	require.ErrorContains(t, CuInterposeContainerConfiguration{}.Validate(), "unsupported multi-GPU preparation")
}

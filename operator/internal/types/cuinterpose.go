// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// MultiGPUPreparation selects who prepares an opted-in SnapshotJob source Pod for
// multi-GPU checkpoints.
type MultiGPUPreparation string

const (
	// MultiGPUPreparationAgent leaves the source Pod unchanged. The agent's NRI
	// plugin prepares the target when it starts, which needs NRI on the node.
	MultiGPUPreparationAgent MultiGPUPreparation = "agent"
	// MultiGPUPreparationInitContainer adds an init container that installs the
	// libraries, and starts the target under the launcher. It works on any runtime.
	MultiGPUPreparationInitContainer MultiGPUPreparation = "initContainer"
)

// CuInterposeContainerConfiguration configures how the operator prepares opted-in
// SnapshotJob sources, and the init container it adds in initContainer mode.
type CuInterposeContainerConfiguration struct {
	Preparation MultiGPUPreparation
	Image       string
	PullPolicy  corev1.PullPolicy
}

// AddsInitContainer reports whether the operator shapes opted-in sources itself.
func (c CuInterposeContainerConfiguration) AddsInitContainer() bool {
	return c.Preparation == MultiGPUPreparationInitContainer
}

func (c CuInterposeContainerConfiguration) Validate() error {
	switch c.Preparation {
	case MultiGPUPreparationAgent:
	case MultiGPUPreparationInitContainer:
		if strings.TrimSpace(c.Image) == "" {
			return fmt.Errorf("cuinterpose image is required when multi-GPU preparation is %s", c.Preparation)
		}
	default:
		return fmt.Errorf("unsupported multi-GPU preparation %q: use %s or %s",
			c.Preparation, MultiGPUPreparationAgent, MultiGPUPreparationInitContainer)
	}
	switch c.PullPolicy {
	case "", corev1.PullAlways, corev1.PullIfNotPresent, corev1.PullNever:
		return nil
	default:
		return fmt.Errorf("unsupported cuinterpose image pull policy %q: use Always, IfNotPresent, Never, or empty", c.PullPolicy)
	}
}

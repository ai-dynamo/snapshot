// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// CuInterposeContainerConfiguration configures the init container that installs the
// cuinterpose libraries and launcher into opted-in SnapshotJob sources.
type CuInterposeContainerConfiguration struct {
	Image      string
	PullPolicy corev1.PullPolicy
}

func (c CuInterposeContainerConfiguration) Validate() error {
	if strings.TrimSpace(c.Image) == "" {
		return fmt.Errorf("cuinterpose image is required")
	}
	switch c.PullPolicy {
	case "", corev1.PullAlways, corev1.PullIfNotPresent, corev1.PullNever:
		return nil
	default:
		return fmt.Errorf("unsupported cuinterpose image pull policy %q: use Always, IfNotPresent, Never, or empty", c.PullPolicy)
	}
}

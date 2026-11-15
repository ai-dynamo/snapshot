// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package types

// The operator labels each resource it writes outside Helm with ManagedByLabel
// set to ManagedByOperator. helm uninstall leaves those resources, and the
// label finds them.
const (
	// ManagedByLabel is the well-known label that names the tool managing a
	// resource.
	ManagedByLabel = "app.kubernetes.io/managed-by"
	// ManagedByOperator is ManagedByLabel's value on what the operator writes.
	ManagedByOperator = "snapshot-operator"
)

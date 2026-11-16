// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package types

// The operator labels the Secrets and ConfigMaps it writes in its own
// namespace, outside Helm, with Labels. helm uninstall leaves them, and the
// labels find them.
const (
	// PartOfLabel is the well-known label that names the application a
	// resource is part of.
	PartOfLabel = "app.kubernetes.io/part-of"
	// PartOfSnapshot is PartOfLabel's value on what the operator writes.
	PartOfSnapshot = "snapshot"
	// ManagedByLabel is the well-known label that names the tool managing a
	// resource.
	ManagedByLabel = "app.kubernetes.io/managed-by"
	// ManagedByOperator is ManagedByLabel's value on what the operator writes.
	ManagedByOperator = "snapshot-operator"
	// InstanceLabel is the well-known label that names the instance of the
	// application a resource belongs to: for the operator, its Helm release.
	InstanceLabel = "app.kubernetes.io/instance"
)

// Labels returns the labels of the Secrets and ConfigMaps the operator writes
// in its own namespace, in a new map that the caller may change.
func Labels() map[string]string {
	return map[string]string{PartOfLabel: PartOfSnapshot, ManagedByLabel: ManagedByOperator}
}

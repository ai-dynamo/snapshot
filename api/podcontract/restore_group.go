// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import "k8s.io/apimachinery/pkg/types"

// RestoreGroupBinding is a restore Pod's side of its binding to a
// PodGroupRestore: the values of its RestoreGroupAnnotation and
// RestoreGroupUIDAnnotation. The operator and the node agent both decide
// through Binds which attempt may act on the Pod.
type RestoreGroupBinding struct {
	// Name is the PodGroupRestore that RestoreGroupAnnotation names, or "".
	Name string
	// UID is the UID that RestoreGroupUIDAnnotation holds, or "".
	UID types.UID
}

// RestoreGroupBindingOf reads a restore Pod's binding from its annotations.
func RestoreGroupBindingOf(annotations map[string]string) RestoreGroupBinding {
	return RestoreGroupBinding{
		Name: annotations[RestoreGroupAnnotation],
		UID:  types.UID(annotations[RestoreGroupUIDAnnotation]),
	}
}

// Binds reports whether the binding binds the Pod to exactly the
// PodGroupRestore with name and uid: both annotations are set, and they equal
// name and uid. A name without a UID binds the Pod to no attempt yet, and a
// name with another UID binds it to another attempt of that name.
func (b RestoreGroupBinding) Binds(name string, uid types.UID) bool {
	return b.Name != "" && b.UID != "" && b.Name == name && b.UID == uid
}

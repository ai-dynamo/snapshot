// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// +kubebuilder:object:generate=true
// +groupName=nvidia.com
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var SchemeGroupVersion = schema.GroupVersion{Group: "nvidia.com", Version: "v1alpha1"}

// GroupVersion is an alias for SchemeGroupVersion, used by callers that build
// typed objects (e.g. the operator setting APIVersion on a PodSnapshotContent).
var GroupVersion = SchemeGroupVersion

func AddToScheme(s *runtime.Scheme) error {
	s.AddKnownTypes(SchemeGroupVersion,
		&PodSnapshot{}, &PodSnapshotList{},
		&PodSnapshotContent{}, &PodSnapshotContentList{},
	)
	metav1.AddToGroupVersion(s, SchemeGroupVersion)
	return nil
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	corev1 "k8s.io/api/core/v1"
)

// TrimPodForCache keeps only the Pod fields the controllers read from the
// cache. The operator watches every Pod in the cluster but never writes one.
func TrimPodForCache(obj any) (any, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return obj, nil
	}
	pod.ManagedFields = nil
	pod.Spec = corev1.PodSpec{NodeName: pod.Spec.NodeName}
	pod.Status = corev1.PodStatus{
		Phase:                 pod.Status.Phase,
		ContainerStatuses:     pod.Status.ContainerStatuses,
		InitContainerStatuses: pod.Status.InitContainerStatuses,
	}
	return pod, nil
}

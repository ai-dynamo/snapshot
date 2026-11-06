// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// TrimPodForCache keeps only the Pod fields the controllers read from the
// cache. The operator watches every Pod in the cluster but never writes one.
// Of the Pod's status it keeps the phase, the address and the container
// statuses, and of its conditions only those a pod-group controller reads:
// Ready, which admission reads from the snapshot agent Pods, and the restore
// conditions that a PodGroupRestore aggregates from its restore Pods.
func TrimPodForCache(obj any) (any, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return obj, nil
	}
	var conditions []corev1.PodCondition
	for _, c := range pod.Status.Conditions {
		switch c.Type {
		case corev1.PodReady,
			podcontract.RestoreStartedCondition,
			podcontract.RestoredCondition,
			podcontract.GroupRestoreFailedCondition:
			conditions = append(conditions, c)
		}
	}
	pod.ManagedFields = nil
	pod.Spec = corev1.PodSpec{NodeName: pod.Spec.NodeName}
	pod.Status = corev1.PodStatus{
		Phase:                 pod.Status.Phase,
		Conditions:            conditions,
		PodIP:                 pod.Status.PodIP,
		ContainerStatuses:     pod.Status.ContainerStatuses,
		InitContainerStatuses: pod.Status.InitContainerStatuses,
	}
	return pod, nil
}

// isPodReady reports whether the Pod's Ready condition is True. TrimPodForCache
// keeps that condition, so this works on cached Pods.
func isPodReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

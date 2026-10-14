// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

func podFromInformerObj(obj interface{}) (*corev1.Pod, bool) {
	if pod, ok := obj.(*corev1.Pod); ok {
		return pod, true
	}
	tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
	if !ok {
		return nil, false
	}
	pod, ok := tombstone.Obj.(*corev1.Pod)
	return pod, ok
}

func isContainerReady(pod *corev1.Pod, containerName string) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == containerName {
			return status.Ready
		}
	}
	return false
}

// core/v1 Event field limits the API server enforces once eventTime is set.
// A node name may be up to 253 characters, so ReportingInstance needs the cap.
const (
	eventMessageLengthLimit           = 1024
	eventReportingInstanceLengthLimit = 128
)

func emitPodEvent(ctx context.Context, clientset kubernetes.Interface, log logr.Logger, pod *corev1.Pod, component, eventType, reason, message string) {
	now := time.Now()
	reportingInstance := pod.Spec.NodeName
	if reportingInstance == "" {
		reportingInstance = component
	}
	reportingInstance = truncateUTF8(reportingInstance, eventReportingInstanceLengthLimit)
	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-", pod.Name),
			Namespace:    pod.Namespace,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:       "Pod",
			Namespace:  pod.Namespace,
			Name:       pod.Name,
			UID:        pod.UID,
			APIVersion: "v1",
		},
		Type:    eventType,
		Reason:  reason,
		Message: truncateEventMessage(message),
		Source: corev1.EventSource{
			Component: component,
		},
		Count:               1,
		FirstTimestamp:      metav1.NewTime(now),
		LastTimestamp:       metav1.NewTime(now),
		EventTime:           metav1.NewMicroTime(now),
		Action:              reason,
		ReportingController: component,
		ReportingInstance:   reportingInstance,
	}

	if _, err := clientset.CoreV1().Events(pod.Namespace).Create(ctx, event, metav1.CreateOptions{}); err != nil {
		log.Error(err, "Failed to create event",
			"pod", fmt.Sprintf("%s/%s", pod.Namespace, pod.Name),
			"reason", reason,
			"message", message,
		)
	}
}

func truncateEventMessage(message string) string {
	if len(message) <= eventMessageLengthLimit {
		return message
	}
	const marker = "..."
	return truncateUTF8(message, eventMessageLengthLimit-len(marker)) + marker
}

// truncateUTF8 cuts s to at most limit bytes without splitting a rune.
func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := s[:limit]
	for len(cut) > 0 && !utf8.RuneStart(s[len(cut)]) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

func setPodCondition(status *corev1.PodStatus, condition corev1.PodCondition) {
	condition.LastTransitionTime = metav1.Now()
	for i := range status.Conditions {
		existing := &status.Conditions[i]
		if existing.Type != condition.Type {
			continue
		}
		if existing.Status == condition.Status && !existing.LastTransitionTime.IsZero() {
			condition.LastTransitionTime = existing.LastTransitionTime
		}
		condition.LastProbeTime = existing.LastProbeTime
		*existing = condition
		return
	}
	status.Conditions = append(status.Conditions, condition)
}

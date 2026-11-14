// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1apply "k8s.io/client-go/applyconfigurations/core/v1"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// publishRetryStatus writes the retry status annotation from the record of
// every destination, once per pass after the workers return. The workers
// restore in parallel, so a write from each would drop the others' entries.
// The agent never reads the annotation back; the Pod's value only spares a
// write that changes nothing.
func (w *NodeController) publishRetryStatus(ctx context.Context, pod *corev1.Pod, plan *restorePlan, results []restoreResult) {
	log := w.log.WithValues("pod", fmt.Sprintf("%s/%s", pod.Namespace, pod.Name))
	nodeUID, ok := w.restorePodNodeUID(ctx, pod, results)
	if !ok {
		return
	}
	status := podcontract.RestoreRetryStatus{}
	for _, mapping := range plan.mappings {
		record, found, err := w.readRestoreAttempts(nodeUID, mapping.Destination)
		if err != nil {
			log.Error(err, "Not publishing the restore retry status, since a record cannot be read", "destination", mapping.Destination)
			return
		}
		if found && record.LastFailure != nil {
			status[mapping.Destination] = podcontract.ContainerRetryStatus{
				Attempt:     record.Attempt,
				RetryLimit:  record.Limit,
				LastFailure: *record.LastFailure,
			}
		}
	}
	if len(status) == 0 {
		return
	}
	value, err := podcontract.FormatRestoreRetryStatus(status)
	if err != nil {
		log.Error(err, "Not publishing the restore retry status")
		return
	}
	if pod.Annotations[podcontract.RestoreRetryStatusAnnotation] == value {
		return
	}
	configuration := corev1apply.Pod(pod.Name, pod.Namespace).
		WithAnnotations(map[string]string{podcontract.RestoreRetryStatusAnnotation: value})
	_, err = w.clientset.CoreV1().Pods(pod.Namespace).Apply(ctx, configuration, metav1.ApplyOptions{
		FieldManager: restoreStatusFieldManager,
		Force:        true,
	})
	if err != nil {
		log.Error(err, "Failed to publish the restore retry status")
		emitPodEvent(ctx, w.clientset, log, pod, snapshotEventComponent, corev1.EventTypeWarning, restoreStatusUpdateFailedReason,
			fmt.Sprintf("Failed to record the restore retry status: %v", err))
	}
}

// restorePodNodeUID returns the Pod's UID on the node, read through a
// container the pass found, or the Pod's own UID when the runtime cannot name
// it, as nodePodUID does. ok is false when the pass found no container, so
// no attempt ran and no record changed.
func (w *NodeController) restorePodNodeUID(ctx context.Context, pod *corev1.Pod, results []restoreResult) (nodeUID string, ok bool) {
	for _, result := range results {
		if result.containerID == "" {
			continue
		}
		ok = true
		if uid, err := w.runtime.ResolveContainerPodUID(ctx, result.containerID); err == nil {
			return uid, true
		}
	}
	return string(pod.UID), ok
}

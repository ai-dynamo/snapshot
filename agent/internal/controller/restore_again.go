// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

const (
	// restoreRepeatedReason is the event on a restore Pod whose restore starts
	// again for a destination container that started again.
	restoreRepeatedReason = "RestoreRepeated"

	// restoreAgainCacheWait bounds how long the restore queue waits for its
	// cache to show a restore that RestoreAgain set back in progress.
	restoreAgainCacheWait = 30 * time.Second
)

// RestoreEnded reports whether the Pod's restore has ended, whatever its
// outcome. A destination container that starts again then is restored again
// by RestoreAgain.
func (w *NodeController) RestoreEnded(pod *corev1.Pod) bool {
	current, err := w.restorePodLister.Pods(pod.Namespace).Get(pod.Name)
	return err == nil && current.UID == pod.UID && current.DeletionTimestamp == nil && isRestoreTerminal(current)
}

// RestoreAgain sets the Pod's ended restore back in progress, so the next pass
// restores the container from the same PodSnapshot. That pass resumes the other
// destinations as after an agent restart: one that holds a restore-complete
// keeps its restore, and only its post-restore action runs again. The status
// write is what lets the new restore outlive an agent restart, so its error
// fails the container's creation.
func (w *NodeController) RestoreAgain(ctx context.Context, pod *corev1.Pod, container string) error {
	current, err := w.restorePodLister.Pods(pod.Namespace).Get(pod.Name)
	if err != nil {
		return fmt.Errorf("read restore Pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	if current.UID != pod.UID {
		return fmt.Errorf("restore Pod %s/%s has UID %s, not %s", pod.Namespace, pod.Name, current.UID, pod.UID)
	}
	current = current.DeepCopy()
	w.restoresStartedAgain.Store(string(current.UID), w.nowFn())
	w.restartedRestoreContainers.Store(restoreContainerKey(current.UID, container), struct{}{})
	message := fmt.Sprintf("Destination %s started again; restoring it again from PodSnapshot %s",
		container, current.Annotations[podcontract.RestoreFromAnnotation])
	if err := w.applyRestoredCondition(ctx, current, corev1.ConditionFalse, podcontract.RestoreReasonInProgress, message); err != nil {
		return fmt.Errorf("set the restore of Pod %s/%s back in progress: %w", pod.Namespace, pod.Name, err)
	}
	emitPodEvent(ctx, w.clientset, w.log, current, snapshotEventComponent, corev1.EventTypeNormal, restoreRepeatedReason, message)
	w.restoreQueue.Add(client.ObjectKeyFromObject(current))
	return nil
}

// restoreAgainPending returns how long the restore queue waits before acting
// on the Pod, while its cache still shows the restore that RestoreAgain set
// back in progress as ended. Acting on that copy would mark the restore
// handled, and the new one would never run. Once the cache shows the new
// restore, the mark an earlier pass left goes too. A status write that failed
// leaves the cache as it was, which is why the wait is bounded.
func (w *NodeController) restoreAgainPending(pod *corev1.Pod) time.Duration {
	startedAgain, found := w.restoresStartedAgain.Load(string(pod.UID))
	if !found {
		return 0
	}
	if isRestoreTerminal(pod) {
		if wait := restoreAgainCacheWait - w.nowFn().Sub(startedAgain.(time.Time)); wait > 0 {
			return wait
		}
	} else {
		w.handledRestores.Delete(string(pod.UID))
	}
	w.restoresStartedAgain.CompareAndDelete(string(pod.UID), startedAgain)
	return 0
}

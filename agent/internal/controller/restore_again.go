// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// restoreRepeatedReason is the event on a restore Pod whose restore starts
// again for a destination container that started again.
const restoreRepeatedReason = "RestoreRepeated"

// restoreStartedAgain is what RestoreAgain leaves for the restore queue.
type restoreStartedAgain struct {
	at time.Time
	// handledCleared is set once a pass has cleared the handled mark that the
	// ended restore left, so a mark set after that, such as after a panic,
	// holds.
	handledCleared bool
}

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
	w.restartedRestoreContainers.Store(restoreContainerKey(current.UID, container), struct{}{})
	message := fmt.Sprintf("Destination %s started again; restoring it again from PodSnapshot %s",
		container, current.Annotations[podcontract.RestoreFromAnnotation])
	err = w.applyRestoredCondition(ctx, current, corev1.ConditionFalse, podcontract.RestoreReasonInProgress, message)
	// Left only after the write, which can land even when it fails: a pass
	// that read the ended restore before the write landed would drop it.
	w.restoresStartedAgain.Store(string(current.UID), restoreStartedAgain{at: w.nowFn()})
	w.restoreQueue.Add(client.ObjectKeyFromObject(current))
	if err != nil {
		return fmt.Errorf("set the restore of Pod %s/%s back in progress: %w", pod.Namespace, pod.Name, err)
	}
	emitPodEvent(ctx, w.clientset, w.log, current, snapshotEventComponent, corev1.EventTypeNormal, restoreRepeatedReason, message)
	return nil
}

// latestRestorePod returns the copy of the Pod that the restore queue acts on.
// After RestoreAgain, the cache can still show the restore that ended. Acting
// on that copy would keep the restore handled, and the new one would never
// run, so until the cache shows the restore back in progress, the Pod is read
// from the API server. The first pass that sees the restore back in progress
// clears the handled mark that the ended restore left.
func (w *NodeController) latestRestorePod(ctx context.Context, cached *corev1.Pod) (*corev1.Pod, error) {
	uid := string(cached.UID)
	value, found := w.restoresStartedAgain.Load(uid)
	if !found {
		return cached, nil
	}
	note := value.(restoreStartedAgain)
	if !isRestoreTerminal(cached) {
		if !note.handledCleared {
			w.handledRestores.Delete(uid)
		}
		w.restoresStartedAgain.CompareAndDelete(uid, note)
		return cached, nil
	}
	current, err := w.clientset.CoreV1().Pods(cached.Namespace).Get(ctx, cached.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if current.UID != cached.UID {
		return nil, apierrors.NewNotFound(corev1.Resource("pods"), cached.Name)
	}
	if isRestoreTerminal(current) {
		w.restoresStartedAgain.CompareAndDelete(uid, note)
	} else if !note.handledCleared {
		w.handledRestores.Delete(uid)
		w.restoresStartedAgain.CompareAndSwap(uid, note, restoreStartedAgain{at: note.at, handledCleared: true})
	}
	return current, nil
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"

	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

const restoreRepeatedReason = "RestoreRepeated"

// restoreRepeatable reports whether a destination container that starts again
// after the Pod's restore succeeded gets a new restore, which a retry limit
// above 0 turns on. Without the restore proxy a container that starts again
// runs its own command, so there is nothing to restore into.
func (w *NodeController) restoreRepeatable(pod *corev1.Pod) bool {
	if w.withoutProxy {
		return false
	}
	limit, err := w.restoreRetryLimit(pod)
	return err == nil && limit > 0
}

// RestoreAgain reports whether the destination container the NRI plugin is
// creating gets a new restore: the Pod's restore succeeded and is repeatable.
// While the restore still runs, the container keeps the restore-complete an
// earlier container left, so its proxy exits and the next container asks
// again.
func (w *NodeController) RestoreAgain(pod *corev1.Pod) bool {
	current, err := w.restorePodLister.Pods(pod.Namespace).Get(pod.Name)
	if err != nil || current.UID != pod.UID {
		return false
	}
	return isRestoreSucceeded(current) && w.restoreRepeatable(current)
}

// restoreRestart is a Pod whose restore succeeded and some of whose
// destination containers started again since.
type restoreRestart struct {
	nodeUID   string
	restarted []string
	// done are the other destinations, which keep their success.
	done []restoreResult
}

// restartedDestinations finds the destination containers that started again
// after the Pod's restore succeeded. A container counts when kubelet started
// it no earlier than the Restored condition last changed, in the second-wide
// time the status keeps, and its control directory holds no outcome, since
// the NRI plugin removed the restore-complete that the restore left. Anything
// short of that is no restart, so a pass never restores over a running
// workload. retry asks for another look when the evidence cannot be read yet.
func (w *NodeController) restartedDestinations(ctx context.Context, pod *corev1.Pod) (restart *restoreRestart, retry bool) {
	if pod.DeletionTimestamp != nil || !isRestoreSucceeded(pod) || !w.restoreRepeatable(pod) {
		return nil, false
	}
	succeeded := findRestoredCondition(pod).LastTransitionTime
	started := map[string]string{}
	for _, status := range pod.Status.ContainerStatuses {
		if running := status.State.Running; running != nil && status.ContainerID != "" && !running.StartedAt.Before(&succeeded) {
			started[status.Name] = snapshotruntime.StripCRIScheme(status.ContainerID)
		}
	}
	if len(started) == 0 {
		return nil, false
	}
	log := w.log.WithValues("pod", fmt.Sprintf("%s/%s", pod.Namespace, pod.Name))
	destinations, err := w.RestoreDestinations(ctx, pod)
	if err != nil {
		log.Error(err, "Cannot resolve the destinations of a restore Pod whose container started again")
		return nil, true
	}
	restart = &restoreRestart{}
	for _, destination := range destinations {
		if containerID := started[destination]; containerID != "" {
			if restart.nodeUID, err = w.runtime.ResolveContainerPodUID(ctx, containerID); err != nil {
				log.Error(err, "Cannot read the Pod's UID on the node for a container that started again", "destination", destination)
				return nil, true
			}
			break
		}
	}
	if restart.nodeUID == "" {
		return nil, false
	}
	for _, destination := range destinations {
		complete, err := w.podControlSentinelExistsFn(restart.nodeUID, destination, podcontract.RestoreCompleteFile)
		if err != nil {
			log.Error(err, "Cannot check the outcome of a destination", "destination", destination)
			return nil, true
		}
		failed, err := w.podControlSentinelExistsFn(restart.nodeUID, destination, podcontract.RestoreFailedFile)
		if err != nil {
			log.Error(err, "Cannot check the outcome of a destination", "destination", destination)
			return nil, true
		}
		switch {
		case complete && !failed:
			restart.done = append(restart.done, restoreResult{
				destination: destination,
				state:       restoreResultSucceeded,
				containerID: restoreContainerIDFromStatus(pod, destination),
			})
		case !complete && !failed && started[destination] != "":
			restart.restarted = append(restart.restarted, destination)
		default:
			log.V(1).Info("Not restoring again: a destination holds an outcome its success does not explain",
				"destination", destination, "restore_complete", complete, "restore_failed", failed)
			return nil, false
		}
	}
	if len(restart.restarted) == 0 {
		return nil, false
	}
	return restart, false
}

// startRestoreOver removes each restarted destination's attempt record, so its
// restore starts at the first attempt with a new deadline, and has the pass
// report the other destinations as succeeded without restoring them. The
// evidence lasts until the pass sets the condition back in progress, so an
// agent that stops in between starts the restore over again.
func (w *NodeController) startRestoreOver(ctx context.Context, pod *corev1.Pod, restart *restoreRestart) error {
	for _, destination := range restart.restarted {
		if err := w.removePodControlFileFn(restart.nodeUID, destination, podcontract.RestoreAttemptsFile); err != nil {
			return fmt.Errorf("remove the attempt record of destination %s: %w", destination, err)
		}
	}
	w.handledRestores.Delete(string(pod.UID))
	forgetPodContainers(&w.finishedRestoreDestinations, pod.UID)
	for _, done := range restart.done {
		w.finishedRestoreDestinations.Store(restoreContainerKey(pod.UID, done.destination), done)
	}
	snapshotName := pod.Annotations[podcontract.RestoreFromAnnotation]
	for _, destination := range restart.restarted {
		emitPodEvent(ctx, w.clientset, w.log, pod, snapshotEventComponent, corev1.EventTypeNormal, restoreRepeatedReason,
			fmt.Sprintf("Destination %s started again after its restore; restoring it again from PodSnapshot %s", destination, snapshotName))
	}
	return nil
}

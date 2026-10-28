// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"

	"github.com/ai-dynamo/snapshot/agent/internal/lifecyclehooks"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

const (
	// reasonInvalidCheckpointReadinessProbe fails a work order whose probe can
	// never be run safely: a file the agent must not read, or an HTTP probe on
	// a host-network Pod.
	reasonInvalidCheckpointReadinessProbe = "InvalidCheckpointReadinessProbe"

	// reasonWaitingForCheckpointReadiness is the Event on the source Pod when
	// the agent starts waiting for its checkpoint readiness probe.
	reasonWaitingForCheckpointReadiness = "WaitingForCheckpointReadiness"
)

// checkpointProbeState counts one work order's probe results. Only the
// goroutine holding the work order's queue key touches it.
//
// It lives in memory only: an agent restart starts the count again, which
// delays the checkpoint but never takes one early.
type checkpointProbeState struct {
	containerID  string
	restartCount int32
	successes    int32
	nextCheck    time.Time
	announced    bool
}

// sourceReadyForCheckpoint decides whether the source container may be
// checkpointed now. Without a checkpointReadinessProbe that is the container's
// Ready condition, as before. With one, the probe decides and Ready is not
// needed: the probe runs every periodSeconds, after initialDelaySeconds from
// the container's start, until it succeeds successThreshold times in a row.
//
// When it returns false, the caller returns err. err is nil unless a terminal
// status could not be written: a refused probe fails the work order here.
func (w *NodeController) sourceReadyForCheckpoint(
	ctx context.Context,
	content *snapshotv1alpha1.PodSnapshotContent,
	pod *corev1.Pod,
	containerName string,
) (bool, error) {
	logger := logr.FromContextOrDiscard(ctx)
	probe := checkpointReadinessProbe(content, containerName)
	if probe == nil {
		if !isContainerReady(pod, containerName) {
			logger.V(1).Info("Source container not ready, awaiting quiesce", "pod", pod.Name, "container", containerName)
			return false, nil
		}
		return true, nil
	}

	if err := checkProbeTarget(pod, containerName, probe); err != nil {
		w.checkpointProbes.Delete(content.Name)
		return false, w.setSnapshotContentFailed(ctx, content, reasonInvalidCheckpointReadinessProbe, err)
	}

	status := containerStatus(pod, containerName)
	if status == nil || status.State.Running == nil {
		// A start is a Pod status change, and that wakes the work order again.
		logger.V(1).Info("Source container not running, awaiting its checkpoint readiness probe", "pod", pod.Name, "container", containerName)
		return false, nil
	}
	state := w.checkpointProbeState(content.Name)
	if state.containerID != status.ContainerID || state.restartCount != status.RestartCount {
		*state = checkpointProbeState{
			containerID:  status.ContainerID,
			restartCount: status.RestartCount,
			nextCheck:    status.State.Running.StartedAt.Add(time.Duration(probe.InitialDelaySeconds) * time.Second),
			announced:    state.announced,
		}
	}

	// Pod events wake the work order more often than the period, so checks
	// are paced by nextCheck, not by how often reconcile runs.
	now := w.now()
	if now.Before(state.nextCheck) {
		w.requeueCapture(content.Name, state.nextCheck.Sub(now))
		return false, nil
	}
	if !state.announced {
		emitPodEvent(ctx, w.clientset, logger, pod, snapshotEventComponent, corev1.EventTypeNormal,
			reasonWaitingForCheckpointReadiness,
			fmt.Sprintf("Waiting for the checkpoint readiness probe of container %q", containerName))
		state.announced = true
	}

	period := time.Duration(probe.PeriodSeconds) * time.Second
	state.nextCheck = now.Add(period)
	if err := w.runCheckpointProbe(ctx, pod, status, probe); err != nil {
		logger.V(1).Info("Checkpoint readiness probe failed", "pod", pod.Name, "container", containerName, "error", err.Error())
		state.successes = 0
	} else {
		state.successes++
	}
	if state.successes >= max(probe.SuccessThreshold, 1) {
		w.checkpointProbes.Delete(content.Name)
		logger.Info("Checkpoint readiness probe passed", "pod", pod.Name, "container", containerName)
		return true, nil
	}
	w.requeueCapture(content.Name, period)
	return false, nil
}

// checkpointReadinessProbe returns the probe the work order saved for the
// container, or nil.
func checkpointReadinessProbe(content *snapshotv1alpha1.PodSnapshotContent, containerName string) *snapshotv1alpha1.CheckpointReadinessProbe {
	for i := range content.Spec.Containers {
		if content.Spec.Containers[i].Name == containerName {
			return content.Spec.Containers[i].CheckpointReadinessProbe
		}
	}
	return nil
}

// checkProbeTarget refuses a probe that can never be run safely. These come
// from the Pod spec, so waiting would not change them.
func checkProbeTarget(pod *corev1.Pod, containerName string, probe *snapshotv1alpha1.CheckpointReadinessProbe) error {
	switch {
	case probe.File != nil:
		if err := lifecyclehooks.CheckPath(pod, containerName, probe.File.Path, false); err != nil {
			return fmt.Errorf("checkpoint readiness probe: %w", err)
		}
	case probe.HTTPGet != nil:
		if pod.Spec.HostNetwork {
			return fmt.Errorf("checkpoint readiness probe: Pod %s/%s uses the host network; HTTP probes need the Pod's own IP", pod.Namespace, pod.Name)
		}
	default:
		return fmt.Errorf("checkpoint readiness probe for container %q has no file or httpGet", containerName)
	}
	return nil
}

// runCheckpointProbe runs one check. Any error is a failed check.
func (w *NodeController) runCheckpointProbe(
	ctx context.Context,
	pod *corev1.Pod,
	status *corev1.ContainerStatus,
	probe *snapshotv1alpha1.CheckpointReadinessProbe,
) error {
	if probe.File != nil {
		pid, _, err := w.runtime.ResolveContainer(ctx, snapshotruntime.StripCRIScheme(status.ContainerID))
		if err != nil {
			return fmt.Errorf("resolve container: %w", err)
		}
		exists, err := w.probeFileFn(pid, probe.File.Path)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%s does not exist", probe.File.Path)
		}
		return nil
	}
	podIP, err := lifecyclehooks.HTTPTarget(pod)
	if err != nil {
		return err
	}
	return w.probeHTTPFn(ctx, podIP, probe.HTTPGet, time.Duration(probe.TimeoutSeconds)*time.Second)
}

func (w *NodeController) checkpointProbeState(name string) *checkpointProbeState {
	state, _ := w.checkpointProbes.LoadOrStore(name, &checkpointProbeState{})
	return state.(*checkpointProbeState)
}

func containerStatus(pod *corev1.Pod, name string) *corev1.ContainerStatus {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == name {
			return &pod.Status.ContainerStatuses[i]
		}
	}
	return nil
}

func (w *NodeController) now() time.Time {
	if w.nowFn != nil {
		return w.nowFn()
	}
	return time.Now()
}

func (w *NodeController) requeueCapture(name string, after time.Duration) {
	if w.requeueCaptureFn != nil {
		w.requeueCaptureFn(name, after)
		return
	}
	w.captureQueue.AddAfter(name, after)
}

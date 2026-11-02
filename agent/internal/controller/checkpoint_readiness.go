// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"

	"github.com/ai-dynamo/snapshot/agent/internal/lifecyclehooks"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// checkpointProbeState counts one work order's probe results. Only the
// goroutine holding the work order's queue key touches it.
type checkpointProbeState struct {
	containerID string
	successes   int32
	nextCheck   time.Time
	announced   bool
}

// track starts the count again for a new source container: on the first
// check, and after a restart, which always gives the container a new ID. The
// first check waits initialDelay from the container's start.
func (s *checkpointProbeState) track(status *corev1.ContainerStatus, initialDelay time.Duration) {
	if s.containerID == status.ContainerID {
		return
	}
	s.containerID = status.ContainerID
	s.successes = 0
	s.nextCheck = status.State.Running.StartedAt.Add(initialDelay)
}

// isSourceReadyForCheckpoint decides whether the source container may be
// checkpointed now. Without a checkpointReadinessProbe that is the container's
// Ready condition, as before. With one, the probe decides and Ready is not
// needed: the probe runs every periodSeconds, after initialDelaySeconds from
// the container's start, until it succeeds successThreshold times in a row.
//
// When it returns false, the caller returns err. err is nil unless a terminal
// status could not be written: a refused probe fails the work order here.
func (w *NodeController) isSourceReadyForCheckpoint(
	ctx context.Context,
	content *snapshotv1alpha1.PodSnapshotContent,
	pod *corev1.Pod,
	containerName string,
) (bool, error) {
	logger := logr.FromContextOrDiscard(ctx)
	probe := findCheckpointReadinessProbe(content, containerName)
	if probe == nil {
		if !isContainerReady(pod, containerName) {
			logger.V(1).Info("Source container not ready, awaiting quiesce", "pod", pod.Name, "container", containerName)
			return false, nil
		}
		return true, nil
	}

	handler, err := w.newCheckpointProbeHandler(probe)
	if err == nil {
		err = handler.check(pod, containerName)
	}
	if err != nil {
		w.checkpointProbes.Delete(content.Name)
		err = fmt.Errorf("checkpoint readiness probe of container %q: %w", containerName, err)
		emitPodEvent(ctx, w.clientset, logger, pod, snapshotEventComponent, corev1.EventTypeWarning,
			reasonInvalidCheckpointReadinessProbe, err.Error())
		return false, w.setSnapshotContentFailed(ctx, content, reasonInvalidCheckpointReadinessProbe, err)
	}

	status := containerStatus(pod, containerName)
	if status == nil || status.State.Running == nil {
		// A start is a Pod status change, and that wakes the work order again.
		logger.V(1).Info("Source container not running, awaiting its checkpoint readiness probe", "pod", pod.Name, "container", containerName)
		return false, nil
	}
	state := w.checkpointProbeState(content.Name)
	state.track(status, time.Duration(probe.InitialDelaySeconds)*time.Second)
	if !state.announced {
		emitPodEvent(ctx, w.clientset, logger, pod, snapshotEventComponent, corev1.EventTypeNormal,
			waitingForCheckpointReadinessReason,
			fmt.Sprintf("Waiting for the checkpoint readiness probe of container %q", containerName))
		state.announced = true
	}

	// Pod events wake the work order more often than the period, so checks
	// are paced by nextCheck, not by how often reconcile runs.
	now := w.nowFn()
	if now.Before(state.nextCheck) {
		w.requeueCaptureFn(content.Name, state.nextCheck.Sub(now))
		return false, nil
	}

	period := time.Duration(probe.PeriodSeconds) * time.Second
	state.nextCheck = now.Add(period)
	if err := handler.probe(ctx, pod, status, time.Duration(probe.TimeoutSeconds)*time.Second); err != nil {
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
	w.requeueCaptureFn(content.Name, period)
	return false, nil
}

// findCheckpointReadinessProbe returns the probe the work order saved for the
// container, or nil.
func findCheckpointReadinessProbe(content *snapshotv1alpha1.PodSnapshotContent, containerName string) *snapshotv1alpha1.CheckpointReadinessProbe {
	for i := range content.Spec.Containers {
		if content.Spec.Containers[i].Name == containerName {
			return content.Spec.Containers[i].CheckpointReadinessProbe
		}
	}
	return nil
}

// checkpointProbeHandler is one kind of checkpoint readiness probe.
type checkpointProbeHandler interface {
	// check refuses a probe that can never run. It reads the Pod spec only,
	// so waiting would not change its answer.
	check(pod *corev1.Pod, containerName string) error
	// probe runs one check. Any error is a failed check.
	probe(ctx context.Context, pod *corev1.Pod, status *corev1.ContainerStatus, timeout time.Duration) error
}

// newCheckpointProbeHandler returns the handler for the probe's kind. It is
// the only place that tells the kinds apart.
func (w *NodeController) newCheckpointProbeHandler(probe *snapshotv1alpha1.CheckpointReadinessProbe) (checkpointProbeHandler, error) {
	switch {
	case probe.File != nil:
		return &fileProbe{exec: w.hookExec, path: probe.File.Path}, nil
	case probe.HTTPGet != nil:
		return &httpProbe{client: w.hookHTTP, action: probe.HTTPGet}, nil
	default:
		return nil, errors.New("it has neither file nor httpGet")
	}
}

// fileProbe succeeds when a regular file exists at path in the source
// container. It reads as the container's user, so any path is allowed.
type fileProbe struct {
	exec lifecyclehooks.Execer
	path string
}

func (p *fileProbe) check(*corev1.Pod, string) error { return nil }

func (p *fileProbe) probe(ctx context.Context, _ *corev1.Pod, status *corev1.ContainerStatus, timeout time.Duration) error {
	exists, err := lifecyclehooks.FileExists(ctx, p.exec, status.ContainerID, p.path, timeout)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%s does not exist", p.path)
	}
	return nil
}

// httpProbe succeeds when a GET to the source Pod's IP returns a 2xx status.
type httpProbe struct {
	client *http.Client
	action *snapshotv1alpha1.HTTPGetAction
}

// check refuses a named port the container doesn't have.
func (p *httpProbe) check(pod *corev1.Pod, containerName string) error {
	_, err := lifecyclehooks.ResolvePort(pod, containerName, p.action.Port)
	return err
}

func (p *httpProbe) probe(ctx context.Context, pod *corev1.Pod, status *corev1.ContainerStatus, timeout time.Duration) error {
	podIP, err := lifecyclehooks.HTTPTarget(pod)
	if err != nil {
		return err
	}
	// check already resolved it, so this only fails if the Pod changed.
	port, err := lifecyclehooks.ResolvePort(pod, status.Name, p.action.Port)
	if err != nil {
		return err
	}
	return lifecyclehooks.HTTPGet(ctx, p.client, lifecyclehooks.RequestURL(podIP, port, p.action.Path), timeout)
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

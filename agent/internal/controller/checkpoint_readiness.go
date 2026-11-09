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

// checkpointGate decides when a work order's source container may be
// checkpointed. getCheckpointGate builds it once per work order; runProbeIfDue
// then runs its handler every period, until it succeeds threshold times in a
// row. Only the goroutine holding the work order's queue key touches it.
type checkpointGate struct {
	handler      checkpointProbeHandler
	initialDelay time.Duration
	// period is the time between checks. Zero checks on every reconcile and
	// never requeues: a Pod event brings the next one.
	period     time.Duration
	timeout    time.Duration
	threshold  int32
	emitEvents bool

	containerID string
	successes   int32
	nextCheck   time.Time
}

// getCheckpointGate returns the work order's gate, building it on the first
// call. Without a checkpointReadinessProbe the gate is the container's Ready
// condition, as before. With one, building it refuses a probe that can never
// run and announces the wait on the source Pod. Both read only the spec, so a
// container restart doesn't rebuild the gate; runProbeIfDue resets its count.
//
// A nil gate means the work order failed here; err is then the error of
// writing that status.
func (w *NodeController) getCheckpointGate(
	ctx context.Context,
	content *snapshotv1alpha1.PodSnapshotContent,
	pod *corev1.Pod,
	containerName string,
) (*checkpointGate, error) {
	if gate, ok := w.checkpointGates.Load(content.Name); ok {
		return gate.(*checkpointGate), nil
	}
	gate, err := newCheckpointGate(findCheckpointReadinessProbe(content, containerName), w.hookExec, w.hookHTTP)
	if err == nil {
		err = gate.handler.Check(pod, containerName)
	}
	logger := logr.FromContextOrDiscard(ctx)
	if err != nil {
		err = fmt.Errorf("checkpoint readiness probe of container %q: %w", containerName, err)
		emitPodEvent(ctx, w.clientset, logger, pod, snapshotEventComponent, corev1.EventTypeWarning,
			reasonInvalidCheckpointReadinessProbe, err.Error())
		return nil, w.setSnapshotContentFailed(ctx, content, reasonInvalidCheckpointReadinessProbe, err)
	}
	if gate.emitEvents {
		emitPodEvent(ctx, w.clientset, logger, pod, snapshotEventComponent, corev1.EventTypeNormal,
			waitingForCheckpointReadinessReason,
			fmt.Sprintf("Waiting for the checkpoint readiness probe of container %q", containerName))
	}
	w.checkpointGates.Store(content.Name, gate)
	return gate, nil
}

// newCheckpointGate returns the gate for probe, or for the Ready condition
// when probe is nil. It is the only place that tells the kinds apart. A file
// probe runs through exec, and an HTTP probe sends through client.
func newCheckpointGate(probe *snapshotv1alpha1.CheckpointReadinessProbe, exec lifecyclehooks.Execer, client *http.Client) (*checkpointGate, error) {
	if probe == nil {
		return &checkpointGate{handler: lifecyclehooks.ReadyCondition{}, threshold: 1}, nil
	}
	gate := &checkpointGate{
		initialDelay: time.Duration(probe.InitialDelaySeconds) * time.Second,
		period:       time.Duration(probe.PeriodSeconds) * time.Second,
		timeout:      time.Duration(probe.TimeoutSeconds) * time.Second,
		threshold:    max(probe.SuccessThreshold, 1),
		emitEvents:   true,
	}
	switch {
	case probe.File != nil:
		gate.handler = &lifecyclehooks.FileProbe{Exec: exec, Path: probe.File.Path}
	case probe.HTTPGet != nil:
		gate.handler = &lifecyclehooks.HTTPProbe{Client: client, Action: probe.HTTPGet}
	default:
		return nil, errors.New("it has neither file nor httpGet")
	}
	return gate, nil
}

// runProbeIfDue runs the handler if a check is due, and reports whether the
// container may be checkpointed. If not, next is when the next check is due;
// zero means a Pod event will wake the work order.
func (g *checkpointGate) runProbeIfDue(ctx context.Context, pod *corev1.Pod, status *corev1.ContainerStatus, now time.Time) (ready bool, next time.Time) {
	logger := logr.FromContextOrDiscard(ctx)
	if status == nil || status.State.Running == nil {
		// A start is a Pod status change, and that wakes the work order again.
		logger.V(1).Info("Source container not running, awaiting its checkpoint readiness", "pod", pod.Name)
		return false, time.Time{}
	}
	g.resetIfNewContainer(status)
	if now.Before(g.nextCheck) {
		return false, g.nextCheck
	}
	g.nextCheck = now.Add(g.period)
	if err := g.handler.Probe(ctx, pod, status, g.timeout); err != nil {
		logger.V(1).Info("Source container not ready for checkpoint", "pod", pod.Name, "container", status.Name, "error", err.Error())
		g.successes = 0
	} else {
		g.successes++
	}
	if g.successes >= g.threshold {
		return true, time.Time{}
	}
	if g.period == 0 {
		return false, time.Time{}
	}
	return false, g.nextCheck
}

// resetIfNewContainer starts the count again for a new source container: on
// the first check, and after a restart, which always gives the container a
// new ID. The first check waits initialDelay from the container's start.
func (g *checkpointGate) resetIfNewContainer(status *corev1.ContainerStatus) {
	if g.containerID == status.ContainerID {
		return
	}
	g.containerID = status.ContainerID
	g.successes = 0
	g.nextCheck = status.State.Running.StartedAt.Add(g.initialDelay)
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

// checkpointProbeHandler is one way to tell that the source container is
// ready for a checkpoint. The lifecyclehooks package implements it: the
// container's Ready condition, a file probe and an HTTP probe.
type checkpointProbeHandler interface {
	// Check refuses a probe that can never run. It reads the Pod spec only,
	// so waiting would not change its answer.
	Check(pod *corev1.Pod, containerName string) error
	// Probe runs one check. Any error is a failed check.
	Probe(ctx context.Context, pod *corev1.Pod, status *corev1.ContainerStatus, timeout time.Duration) error
}

func containerStatus(pod *corev1.Pod, name string) *corev1.ContainerStatus {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == name {
			return &pod.Status.ContainerStatuses[i]
		}
	}
	return nil
}

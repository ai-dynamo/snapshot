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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	"github.com/ai-dynamo/snapshot/agent/internal/lifecyclehooks"
	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// errRestoreInterrupted is a restore whose post-restore action the agent
// stopped before it finished. The restore stays in progress, so the next
// agent resumes the action from restore-complete; it is not an outcome.
var errRestoreInterrupted = errors.New("the agent stopped before the post-restore action finished")

// postRestoreHandler is one kind of lifecycle.postRestore action. The
// lifecyclehooks package implements it: a file action and an HTTP action. A
// restore without an action has no handler.
type postRestoreHandler interface {
	// Check refuses an action that can never run in container. It reads the
	// Pod spec only, so the restore preflight runs it before any CRIU work.
	Check(pod *corev1.Pod, container string) error
	// Notify tells the application once, after restore-complete;
	// retryNotify repeats it.
	Notify(ctx context.Context, target lifecyclehooks.PostRestoreTarget) error
}

// postRestoreAction returns the lifecycle.postRestore action saved with the
// checkpoint for the source container, or nil. Restore reads it from the
// PodSnapshotContent only, so every restore of a checkpoint uses the settings
// it was taken with.
func postRestoreAction(content *snapshotv1alpha1.PodSnapshotContent, sourceContainer string) *snapshotv1alpha1.PostRestoreAction {
	for i := range content.Spec.Containers {
		c := &content.Spec.Containers[i]
		if c.Name == sourceContainer && c.Lifecycle != nil {
			return c.Lifecycle.PostRestore
		}
	}
	return nil
}

// newPostRestoreHandler returns the handler for the action's kind and the
// time between its attempts. It is the only place that tells the kinds apart.
// A file action runs through exec, and an HTTP action sends through client.
// Without an action, the handler is nil.
func newPostRestoreHandler(action *snapshotv1alpha1.PostRestoreAction, exec lifecyclehooks.Execer, client *http.Client) (postRestoreHandler, time.Duration, error) {
	if action == nil {
		return nil, 0, nil
	}
	period := time.Duration(max(action.PeriodSeconds, 1)) * time.Second
	timeout := time.Duration(max(action.TimeoutSeconds, 1)) * time.Second
	switch {
	case action.File != nil:
		return &lifecyclehooks.FilePostRestore{Exec: exec, Path: action.File.Path, Timeout: timeout}, period, nil
	case action.HTTPPost != nil:
		return &lifecyclehooks.HTTPPostRestore{Client: client, Action: action.HTTPPost, Timeout: timeout}, period, nil
	default:
		return nil, 0, errors.New("lifecycle.postRestore has neither file nor httpPost")
	}
}

// planPostRestore builds the restore's post-restore handler and refuses an
// action that can never run in one of the destination containers, so such an
// action restores nothing.
func planPostRestore(
	pod *corev1.Pod,
	mappings []podcontract.ContainerMapping,
	action *snapshotv1alpha1.PostRestoreAction,
	exec lifecyclehooks.Execer,
	client *http.Client,
) (postRestoreHandler, time.Duration, error) {
	handler, period, err := newPostRestoreHandler(action, exec, client)
	if err != nil {
		return nil, 0, postRestoreStepError(executor.StepCheckPostRestore, err)
	}
	if handler == nil {
		return nil, 0, nil
	}
	for _, mapping := range mappings {
		if err := handler.Check(pod, mapping.Destination); err != nil {
			return nil, 0, postRestoreStepError(executor.StepCheckPostRestore,
				fmt.Errorf("container %q: %w", mapping.Destination, err))
		}
	}
	return handler, period, nil
}

// notifyPostRestore tells the application, every postRestorePeriod until it
// succeeds or ctx ends at the restore deadline.
func (op *restoreOperation) notifyPostRestore(ctx context.Context) error {
	if op.postRestore == nil {
		return nil
	}
	target := op.postRestoreTarget()
	return retryNotify(logr.NewContext(ctx, op.log), op.log, op.postRestorePeriod, op.controller.postRestoreWaitFn,
		func(ctx context.Context) error { return op.postRestore.Notify(ctx, target) })
}

func (op *restoreOperation) postRestoreTarget() lifecyclehooks.PostRestoreTarget {
	return lifecyclehooks.PostRestoreTarget{
		Pod:         op.pod,
		Container:   op.destination,
		ContainerID: op.containerID,
		PodIP:       op.podIP,
	}
}

// retryNotify runs notifyFn every period until it succeeds, or until waitFn
// fails because ctx ended. Every failure is retried: a 404 or a missing
// directory may be an application that is still starting.
func retryNotify(
	ctx context.Context,
	log logr.Logger,
	period time.Duration,
	waitFn func(context.Context, time.Duration) error,
	notifyFn func(context.Context) error,
) error {
	for n := 1; ; n++ {
		err := notifyFn(ctx)
		if err == nil {
			log.Info("Post-restore action succeeded", "attempts", n)
			return nil
		}
		log.V(1).Info("Post-restore action failed; retrying", "attempt", n, "error", err.Error())
		if waitErr := waitFn(ctx, period); waitErr != nil {
			return postRestoreStepError(executor.StepPostRestore,
				fmt.Errorf("no success before the restore deadline after %d attempt(s); last error: %w", n, err))
		}
	}
}

// failPostRestore fails the restore after its post-restore action failed,
// unless ctx ended: the agent is shutting down, and writing restore-failed or
// killing the container would keep the next agent from resuming the action.
// The restore deadline is a context of its own under ctx, so a missed
// deadline still fails the restore.
func (op *restoreOperation) failPostRestore(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		op.log.Info("Agent stopping; the next agent resumes the post-restore action", "error", err.Error())
		return errRestoreInterrupted
	}
	return op.failRestore(ctx, err)
}

func postRestoreStepError(step executor.Step, err error) error {
	return &executor.StepError{Step: step, Err: err}
}

// podIP returns the restore Pod's IP, reading the Pod again when the copy this
// pass started with doesn't have one yet.
func (op *restoreOperation) podIP(ctx context.Context) (string, error) {
	if ip, err := lifecyclehooks.HTTPTarget(op.pod); err == nil {
		return ip, nil
	}
	pod, err := op.controller.clientset.CoreV1().Pods(op.pod.Namespace).Get(ctx, op.pod.Name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read restore Pod: %w", err)
	}
	return lifecyclehooks.HTTPTarget(pod)
}

// waitOrDone waits d, or until ctx ends.
func waitOrDone(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

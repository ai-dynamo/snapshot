// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	"github.com/ai-dynamo/snapshot/agent/internal/lifecyclehooks"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

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

func postRestoreStepError(step executor.Step, err error) error {
	return &executor.StepError{Step: step, Err: err}
}

// checkPostRestoreBeforeRestore runs before any CRIU work, so a restore that
// can't notify its application fails without restoring anything. A file
// action needs a path the agent may write and no file there yet: an existing
// file would be taken for the notification, so the application would never
// get one.
func (op *restoreOperation) checkPostRestoreBeforeRestore(ctx context.Context, action *snapshotv1alpha1.PostRestoreAction) error {
	switch {
	case action.File != nil:
		if err := lifecyclehooks.CheckPath(op.pod, op.destination, action.File.Path, true); err != nil {
			return postRestoreStepError(executor.StepCheckPostRestore, err)
		}
		hostPID, _, err := op.controller.runtime.ResolveContainer(ctx, op.containerID)
		if err != nil {
			return postRestoreStepError(executor.StepCheckPostRestore, fmt.Errorf("resolve restore container: %w", err))
		}
		return op.refuseExistingMarker(hostPID, action.File.Path, "before restore")
	case action.HTTPPost != nil:
		if op.pod.Spec.HostNetwork {
			return postRestoreStepError(executor.StepCheckPostRestore,
				fmt.Errorf("Pod %s/%s uses the host network; HTTP actions need the Pod's own IP", op.pod.Namespace, op.pod.Name))
		}
		// A named port resolves against the destination container, which
		// runs the restored application.
		if _, err := lifecyclehooks.ResolvePort(op.pod, op.destination, action.HTTPPost.Port); err != nil {
			return postRestoreStepError(executor.StepCheckPostRestore, err)
		}
		return nil
	default:
		return postRestoreStepError(executor.StepCheckPostRestore, errors.New("lifecycle.postRestore has no file or httpPost"))
	}
}

// checkPostRestoreAfterRestore looks for the marker again once CRIU is done and
// before restore-complete: the rootfs diff applied during restore can bring
// the file back from the source, for example when the source was itself
// restored and notified.
func (op *restoreOperation) checkPostRestoreAfterRestore(action *snapshotv1alpha1.PostRestoreAction, hostPID int) error {
	if action.File == nil {
		return nil
	}
	return op.refuseExistingMarker(hostPID, action.File.Path, "after the restored files were applied")
}

func (op *restoreOperation) refuseExistingMarker(hostPID int, path, when string) error {
	exists, err := op.controller.probeFileFn(hostPID, path)
	if err != nil {
		return postRestoreStepError(executor.StepCheckPostRestore, err)
	}
	if exists {
		return postRestoreStepError(executor.StepCheckPostRestore, fmt.Errorf(
			"%s already exists in container %s %s; the post-restore file must not exist before Snapshot creates it",
			path, op.destination, when))
	}
	return nil
}

// runPostRestore notifies the restored application, after restore-complete,
// every periodSeconds until it succeeds or ctx ends at the restore deadline.
// Every failure is retried: a 404 or a missing directory may be an
// application that is still starting.
//
// A file action counts an existing marker as its own. The marker was checked
// absent before and after CRIU, and only this step creates it, so finding it
// here means an earlier run created it and the agent restarted before
// recording success.
func (op *restoreOperation) runPostRestore(ctx context.Context, action *snapshotv1alpha1.PostRestoreAction) error {
	w := op.controller
	period := time.Duration(max(action.PeriodSeconds, 1)) * time.Second
	timeout := time.Duration(max(action.TimeoutSeconds, 1)) * time.Second
	for attempt := 1; ; attempt++ {
		err := op.postRestoreAttempt(ctx, action, timeout)
		if err == nil {
			op.log.Info("Post-restore action succeeded", "attempts", attempt)
			return nil
		}
		op.log.V(1).Info("Post-restore action failed; retrying", "attempt", attempt, "error", err.Error())
		if waitErr := w.postRestoreWait(ctx, period); waitErr != nil {
			return postRestoreStepError(executor.StepPostRestore,
				fmt.Errorf("no success before the restore deadline after %d attempt(s); last error: %w", attempt, err))
		}
	}
}

func (op *restoreOperation) postRestoreAttempt(ctx context.Context, action *snapshotv1alpha1.PostRestoreAction, timeout time.Duration) error {
	w := op.controller
	if action.File != nil {
		hostPID, _, err := w.runtime.ResolveContainer(ctx, op.containerID)
		if err != nil {
			return fmt.Errorf("resolve restore container: %w", err)
		}
		err = w.createMarkerFn(hostPID, action.File.Path)
		if errors.Is(err, lifecyclehooks.ErrExists) {
			op.log.Info("Post-restore file already exists; an earlier attempt created it", "path", action.File.Path)
			return nil
		}
		return err
	}
	podIP, err := op.podIP(ctx)
	if err != nil {
		return err
	}
	port, err := lifecyclehooks.ResolvePort(op.pod, op.destination, action.HTTPPost.Port)
	if err != nil {
		return err
	}
	return w.postRestoreHTTPFn(ctx, podIP, port, action.HTTPPost.Path, timeout)
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

func (w *NodeController) postRestoreWait(ctx context.Context, d time.Duration) error {
	if w.postRestoreWaitFn != nil {
		return w.postRestoreWaitFn(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

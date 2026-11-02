// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
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

// postRestoreHandler is one kind of lifecycle.postRestore action. Every
// restore plan holds one; without an action it is noPostRestore, so the
// restore needs no checks for a missing action.
type postRestoreHandler interface {
	// check runs before any CRIU work, so an action that can never run
	// restores nothing. It reads the Pod spec only: the container's files
	// aren't restored yet.
	check(ctx context.Context, op *restoreOperation) error
	// recheck runs once the restored files are in place, before
	// restore-complete releases the restored process.
	recheck(ctx context.Context, op *restoreOperation) error
	// notify tells the application, after restore-complete, every
	// periodSeconds until it succeeds or ctx ends at the restore deadline.
	notify(ctx context.Context, op *restoreOperation) error
}

// newPostRestoreHandler returns the handler for the action's kind. It is the
// only place that tells the kinds apart.
func (w *NodeController) newPostRestoreHandler(action *snapshotv1alpha1.PostRestoreAction) postRestoreHandler {
	if action == nil {
		return noPostRestore{}
	}
	retry := postRestoreRetry{
		period:  time.Duration(max(action.PeriodSeconds, 1)) * time.Second,
		timeout: time.Duration(max(action.TimeoutSeconds, 1)) * time.Second,
	}
	switch {
	case action.File != nil:
		return &filePostRestore{postRestoreRetry: retry, exec: w.hookExec, path: action.File.Path}
	case action.HTTPPost != nil:
		return &httpPostRestore{postRestoreRetry: retry, client: w.hookHTTP, action: action.HTTPPost}
	default:
		return invalidPostRestore{errors.New("lifecycle.postRestore has neither file nor httpPost")}
	}
}

// noPostRestore is a checkpoint without a post-restore action.
type noPostRestore struct{}

func (noPostRestore) check(context.Context, *restoreOperation) error   { return nil }
func (noPostRestore) recheck(context.Context, *restoreOperation) error { return nil }
func (noPostRestore) notify(context.Context, *restoreOperation) error  { return nil }

// invalidPostRestore is an action that can never run. Its check fails the
// restore before any CRIU work, as any refused action does.
type invalidPostRestore struct{ err error }

func (h invalidPostRestore) check(context.Context, *restoreOperation) error {
	return postRestoreStepError(executor.StepCheckPostRestore, h.err)
}
func (invalidPostRestore) recheck(context.Context, *restoreOperation) error { return nil }
func (invalidPostRestore) notify(context.Context, *restoreOperation) error  { return nil }

// filePostRestore creates a file in the destination container, as the
// container's user.
type filePostRestore struct {
	postRestoreRetry
	exec lifecyclehooks.Execer
	path string
}

// check refuses one of Snapshot's own control files.
func (h *filePostRestore) check(_ context.Context, op *restoreOperation) error {
	if err := lifecyclehooks.CheckWritePath(op.pod, op.destination, h.path); err != nil {
		return postRestoreStepError(executor.StepCheckPostRestore, err)
	}
	return nil
}

// recheck tests the file only once the restored files are in place, since
// they can bring the file or its directory. The file must not exist yet: an
// existing file would be taken for the notification, so the application
// would never get one. And it must be possible to create: its directory
// exists and the container's user may write it, so a read-only mount fails
// the restore here rather than after the application was released.
func (h *filePostRestore) recheck(ctx context.Context, op *restoreOperation) error {
	err := lifecyclehooks.CheckMarkerTarget(ctx, h.exec, op.containerID, h.path, h.timeout)
	if errors.Is(err, fs.ErrExist) {
		return postRestoreStepError(executor.StepCheckPostRestore, fmt.Errorf(
			"%s already exists in container %s once the restored files are in place; "+
				"the post-restore file must not exist before Snapshot creates it",
			h.path, op.destination))
	}
	if err != nil {
		return postRestoreStepError(executor.StepCheckPostRestore, err)
	}
	return nil
}

// notify counts an existing file as its own. recheck found no file before
// restore-complete, and only this step creates it, so finding it here means
// an earlier run created it and the agent restarted before recording
// success.
func (h *filePostRestore) notify(ctx context.Context, op *restoreOperation) error {
	return h.retry(ctx, op, func(ctx context.Context) error {
		err := lifecyclehooks.CreateMarker(ctx, h.exec, op.containerID, h.path, h.timeout)
		if errors.Is(err, fs.ErrExist) {
			op.log.Info("Post-restore file already exists; an earlier attempt created it", "path", h.path)
			return nil
		}
		return err
	})
}

// httpPostRestore sends a POST to the restore Pod's IP.
type httpPostRestore struct {
	postRestoreRetry
	client *http.Client
	action *snapshotv1alpha1.HTTPPostAction
}

// check refuses a port name the destination container doesn't have. A named
// port resolves against the destination, which runs the restored application.
func (h *httpPostRestore) check(_ context.Context, op *restoreOperation) error {
	if _, err := lifecyclehooks.ResolvePort(op.pod, op.destination, h.action.Port); err != nil {
		return postRestoreStepError(executor.StepCheckPostRestore, err)
	}
	return nil
}

func (h *httpPostRestore) recheck(context.Context, *restoreOperation) error { return nil }

func (h *httpPostRestore) notify(ctx context.Context, op *restoreOperation) error {
	return h.retry(ctx, op, func(ctx context.Context) error {
		podIP, err := op.podIP(ctx)
		if err != nil {
			return err
		}
		port, err := lifecyclehooks.ResolvePort(op.pod, op.destination, h.action.Port)
		if err != nil {
			return err
		}
		return lifecyclehooks.HTTPPost(ctx, h.client, lifecyclehooks.RequestURL(podIP, port, h.action.Path), h.timeout)
	})
}

// postRestoreRetry runs an action's attempts. Every failure is retried: a 404
// or a missing directory may be an application that is still starting.
type postRestoreRetry struct {
	period, timeout time.Duration
}

func (r postRestoreRetry) retry(ctx context.Context, op *restoreOperation, attempt func(context.Context) error) error {
	for n := 1; ; n++ {
		err := attempt(ctx)
		if err == nil {
			op.log.Info("Post-restore action succeeded", "attempts", n)
			return nil
		}
		op.log.V(1).Info("Post-restore action failed; retrying", "attempt", n, "error", err.Error())
		if waitErr := op.controller.postRestoreWaitFn(ctx, r.period); waitErr != nil {
			return postRestoreStepError(executor.StepPostRestore,
				fmt.Errorf("no success before the restore deadline after %d attempt(s); last error: %w", n, err))
		}
	}
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

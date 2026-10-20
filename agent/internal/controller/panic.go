// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// panicStatusRetries bounds the attempts to record a recovered panic. The
// queue does not redeliver a panicking key, so this is the only chance.
const panicStatusRetries = 3

// recoveredPanicError carries a panic as an error, so the normal failure
// paths can report it.
type recoveredPanicError struct{ value any }

func (e *recoveredPanicError) Error() string { return fmt.Sprintf("panic: %v", e.value) }

// recoverPanic hands a panic to onPanic as an error and logs it with its stack.
func recoverPanic(log logr.Logger, operation string, onPanic func(error), keysAndValues ...any) {
	r := recover()
	if r == nil {
		return
	}
	err := &recoveredPanicError{value: r}
	log.Error(err, "Recovered panic in "+operation,
		append(append([]any{}, keysAndValues...), "stack", string(debug.Stack()))...)
	if onPanic != nil {
		onPanic(err)
	}
}

// failCaptureAfterPanic fails the work order, so a capture whose goroutine
// died does not sit pending until its lease expires.
func (w *NodeController) failCaptureAfterPanic(ctx context.Context, name string, cause error) {
	defer recoverPanic(w.log, "capture panic cleanup", nil, "content", name)

	for attempt := range panicStatusRetries {
		content := &snapshotv1alpha1.PodSnapshotContent{}
		if err := w.client.Get(ctx, client.ObjectKey{Name: name}, content); err != nil {
			w.log.Error(err, "Failed to read work order after recovered panic", "content", name)
			return
		}
		if isContentTerminal(content) {
			return
		}
		err := w.setSnapshotContentFailed(ctx, content, "AgentPanic", errors.New(cause.Error()))
		if err == nil {
			return
		}
		if attempt == panicStatusRetries-1 {
			w.log.Error(err, "Failed to record recovered panic on work order", "content", name)
		}
	}
}

// failRestoreAfterPanic records a recovered panic on the restore pod. Without
// it the pod carries no condition and the key is redelivered on every update,
// repeating the panic.
func (w *NodeController) failRestoreAfterPanic(ctx context.Context, key client.ObjectKey, pod *corev1.Pod, cause error) {
	defer recoverPanic(w.log, "restore panic cleanup", nil, "pod", key.String())

	if pod == nil {
		return
	}
	// Marked handled first: a failed status write must not leave the key
	// eligible for redelivery into the same panic.
	w.markRestoreHandled(pod)
	message := cause.Error() + "; see the node agent log on " + w.config.NodeName
	for attempt := range panicStatusRetries {
		err := w.applyRestoredCondition(ctx, pod, corev1.ConditionFalse, podcontract.RestoreReasonFailed, message)
		if err == nil {
			return
		}
		if attempt == panicStatusRetries-1 {
			w.log.Error(err, "Failed to record recovered panic on restore pod", "pod", key.String())
		}
	}
}

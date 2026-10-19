// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// stackLimit caps the stack recorded for a recovered panic.
const stackLimit = 4096

// recoveredPanicError carries a panic as an error, so the normal failure
// paths can report it.
type recoveredPanicError struct {
	value any
	stack string
}

func (e *recoveredPanicError) Error() string {
	return fmt.Sprintf("panic: %v", e.value)
}

// Stack returns the captured stack, truncated to stackLimit.
func (e *recoveredPanicError) Stack() string { return e.stack }

// recoverPanic hands a panic to onPanic as an error and logs it with its stack.
func recoverPanic(log logr.Logger, operation string, onPanic func(error), keysAndValues ...any) {
	r := recover()
	if r == nil {
		return
	}
	stack := string(debug.Stack())
	err := &recoveredPanicError{value: r, stack: truncateStack(stack)}
	log.Error(err, "Recovered panic in "+operation,
		append(append([]any{}, keysAndValues...), "stack", stack)...)
	if onPanic != nil {
		onPanic(err)
	}
}

func truncateStack(stack string) string {
	if len(stack) <= stackLimit {
		return stack
	}
	return stack[:stackLimit] + "\n...<truncated>"
}

// failCaptureAfterPanic fails the work order, so a capture whose goroutine
// died does not sit pending until its lease expires.
func (w *NodeController) failCaptureAfterPanic(ctx context.Context, name string, cause error) {
	defer recoverPanic(w.log, "capture panic cleanup", nil, "content", name)

	content := &snapshotv1alpha1.PodSnapshotContent{}
	if err := w.client.Get(ctx, client.ObjectKey{Name: name}, content); err != nil {
		w.log.Error(err, "Failed to read work order after recovered panic", "content", name)
		return
	}
	if isContentTerminal(content) {
		return
	}
	message := cause.Error()
	if panicErr, ok := cause.(*recoveredPanicError); ok {
		message = fmt.Sprintf("%s\n%s", panicErr.Error(), panicErr.Stack())
	}
	if err := w.setSnapshotContentFailed(ctx, content, "AgentPanic", errors.New(message)); err != nil {
		w.log.Error(err, "Failed to record recovered panic on work order", "content", name)
	}
}

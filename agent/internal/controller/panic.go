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

// stackLimit caps the stack recorded for a recovered panic. The full trace goes
// to the agent log; this bound is for what a caller may copy into an API
// status, which must not carry an unbounded blob.
const stackLimit = 4096

// recoveredPanicError carries a recovered panic as an error so the normal
// failure paths can report it like any other.
type recoveredPanicError struct {
	value any
	stack string
}

func (e *recoveredPanicError) Error() string {
	return fmt.Sprintf("panic: %v", e.value)
}

// Stack returns the captured stack, truncated to stackLimit.
func (e *recoveredPanicError) Stack() string { return e.stack }

// recoverPanic turns a panic in a worker goroutine into an error handed to
// onPanic, and logs it with its stack.
//
// The agent is a single process per node serving every capture and every
// restore on that node, and these workers run as bare goroutines: without
// this, one malformed work order takes every unrelated in-flight operation
// down with it. Recovering keeps the blast radius at the one work order that
// panicked. See SNEP-394.
//
// Call it deferred, before any other defer that must still run:
//
//	defer recoverPanic(log, "capture", func(err error) { ... }, "content", name)
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

// failCaptureAfterPanic records a recovered panic as a terminal failure on the
// work order, so a capture whose goroutine died does not sit pending until its
// lease expires. Best-effort: the agent has already survived the panic, and a
// failed status write must not raise a second one.
//
// The condition message carries the panic value and a truncated stack. The
// full trace is in the agent log, which is operator-scoped; the status is
// readable by anyone who can read the work order, so it stays bounded.
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

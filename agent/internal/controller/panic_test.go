// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
)

func TestRecoverPanicHandsTheCauseToTheHandler(t *testing.T) {
	var got error
	func() {
		defer recoverPanic(logr.Discard(), "test", func(err error) { got = err })
		panic("boom")
	}()

	if got == nil {
		t.Fatal("recoverPanic did not invoke the handler")
	}
	if !strings.Contains(got.Error(), "boom") {
		t.Errorf("error %q does not name the panic value", got)
	}
	var panicErr *recoveredPanicError
	if !errors.As(got, &panicErr) {
		t.Fatalf("error %T is not a *recoveredPanicError", got)
	}
}

// The condition built from this error is readable by anyone who can read the
// work order, so it must not carry a stack.
func TestRecoveredPanicErrorCarriesNoStack(t *testing.T) {
	err := &recoveredPanicError{value: "boom"}
	if got := err.Error(); got != "panic: boom" {
		t.Errorf("Error() = %q, want %q", got, "panic: boom")
	}
	if strings.Contains(err.Error(), ".go:") || strings.Contains(err.Error(), "goroutine") {
		t.Errorf("Error() leaks a stack: %q", err.Error())
	}
}

func TestRecoverPanicIsInertWithoutAPanic(t *testing.T) {
	called := false
	func() {
		defer recoverPanic(logr.Discard(), "test", func(error) { called = true })
	}()
	if called {
		t.Error("handler ran without a panic")
	}
}

func TestRecoverPanicContainsThePanic(t *testing.T) {
	survived := false
	func() {
		defer func() { survived = true }()
		defer recoverPanic(logr.Discard(), "test", nil)
		panic(errors.New("work order is malformed"))
	}()
	if !survived {
		t.Error("panic escaped recoverPanic")
	}
}

// restorePodContainers runs each destination under sync.WaitGroup.Go, which
// recovers only to re-panic, so a panic there would kill the agent. The worker
// seeds a failed result before running, so a recovered panic leaves a reported
// failure rather than a zero-valued success.
func TestRestoreDestinationPanicLeavesAFailedResult(t *testing.T) {
	results := make([]restoreResult, 1)
	var workers sync.WaitGroup

	workers.Go(func() {
		results[0] = restoreResult{destination: "main", state: restoreResultFailed,
			reason: "agent panicked restoring this container; see the node agent log"}
		defer recoverPanic(logr.Discard(), "restore destination", nil)
		panic("restore blew up")
	})
	workers.Wait()

	if results[0].state != restoreResultFailed {
		t.Errorf("state = %v, want restoreResultFailed", results[0].state)
	}
	if results[0].destination != "main" {
		t.Errorf("destination = %q, want main", results[0].destination)
	}
	if results[0].reason == "" {
		t.Error("a panicking destination reported no reason")
	}
}

// The seeded result must not survive a destination that completes.
func TestRestoreDestinationSuccessOverwritesTheSeed(t *testing.T) {
	results := make([]restoreResult, 1)
	var workers sync.WaitGroup

	workers.Go(func() {
		results[0] = restoreResult{destination: "main", state: restoreResultFailed, reason: "seed"}
		defer recoverPanic(logr.Discard(), "restore destination", nil)
		results[0] = restoreResult{destination: "main", state: restoreResultSucceeded}
	})
	workers.Wait()

	if results[0].state != restoreResultSucceeded {
		t.Errorf("state = %v, want restoreResultSucceeded", results[0].state)
	}
}

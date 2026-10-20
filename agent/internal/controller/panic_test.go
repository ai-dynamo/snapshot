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

// This text reaches a status condition readable by anyone who can read the
// work order, so the exact form is the point: a stack must not creep into it.
func TestRecoveredPanicErrorCarriesNoStack(t *testing.T) {
	err := &recoveredPanicError{value: "boom"}
	if got := err.Error(); got != "panic: boom" {
		t.Errorf("Error() = %q, want %q", got, "panic: boom")
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
// recovers only to re-panic, so a deferred recover inside the worker is what
// keeps a panicking restore from killing the agent. This pins that the idiom
// contains the panic; the wiring in restorePodContainers is not reachable here
// without a plan, a pod and CRIU.
func TestRecoverPanicContainsAWaitGroupWorker(t *testing.T) {
	var workers sync.WaitGroup
	workers.Go(func() {
		defer recoverPanic(logr.Discard(), "restore destination", nil)
		panic("restore blew up")
	})
	workers.Wait() // re-panics, and so fails the run, without the recover above
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"strings"
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
	if panicErr.Stack() == "" {
		t.Error("recovered panic carries no stack")
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

func TestRecoveredPanicStackIsBounded(t *testing.T) {
	huge := strings.Repeat("x", stackLimit*3)
	got := truncateStack(huge)
	if len(got) <= stackLimit {
		t.Fatalf("truncated stack length %d, want just over %d", len(got), stackLimit)
	}
	if len(got) > stackLimit+64 {
		t.Errorf("truncated stack length %d exceeds the bound", len(got))
	}
	if !strings.HasSuffix(got, "<truncated>") {
		t.Error("truncated stack is not marked as truncated")
	}
}

func TestTruncateStackLeavesShortStacksAlone(t *testing.T) {
	short := "goroutine 1 [running]:\nmain.main()"
	if got := truncateStack(short); got != short {
		t.Errorf("truncateStack(%q) = %q, want it unchanged", short, got)
	}
}

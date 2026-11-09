// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import "errors"

// RestoreFailure is a failed restore attempt: where and why it failed, and
// what it leaves for another attempt.
type RestoreFailure struct {
	// Step and Cause come from failStep. A typed cause keeps its type, such as
	// a *pagebroker.FailureError or a *compat.IncompatibleError.
	Step  Step
	Cause error
	// Final is set when no other attempt can succeed, or none can start
	// safely, such as after an abort that left the transaction open.
	Final bool
	// ContainerChanged is set when the attempt changed the destination
	// container, so another attempt needs a new container.
	ContainerChanged bool
	// Cleanup holds the unmount and abort failures. It is reported, and never
	// read as the cause.
	Cleanup error
}

func (f *RestoreFailure) Error() string { return errors.Join(f.Unwrap()...).Error() }

func (f *RestoreFailure) Unwrap() []error {
	attempt := f.Cause
	if f.Step != "" {
		attempt = failStep(f.Step, f.Cause)
	}
	if f.Cleanup == nil {
		return []error{attempt}
	}
	return []error{attempt, f.Cleanup}
}

func newRestoreFailure(err, cleanup error, abortFailed, mountLeft bool) *RestoreFailure {
	failure := &RestoreFailure{
		Step:             StepOf(err),
		Cause:            err,
		Final:            abortFailed || isFinal(err),
		ContainerChanged: mountLeft || containerChanged(err),
	}
	var stepErr *StepError
	if errors.As(err, &stepErr) {
		failure.Cause = stepErr.Err
	}
	if cleanup != nil {
		failure.Cleanup = CleanupFailure(cleanup)
	}
	return failure
}

// finalError marks a failure no other attempt can fix.
type finalError struct {
	err error
}

func (e *finalError) Error() string { return e.err.Error() }

func (e *finalError) Unwrap() error { return e.err }

func final(err error) error { return &finalError{err: err} }

func isFinal(err error) bool {
	var marked *finalError
	return errors.As(err, &marked)
}

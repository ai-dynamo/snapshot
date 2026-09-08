// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"errors"
	"fmt"
)

var errMessageTooLarge = fmt.Errorf("message exceeds %d bytes", maxMessageSize)

// transportError marks a request whose outcome is unknown: the connection failed
// before a response was read. Commit retries these; staging requests do not.
type transportError struct {
	cause error
}

func (e transportError) Error() string { return e.cause.Error() }

func (e transportError) Unwrap() error { return e.cause }

func isTransportError(err error) bool {
	var transport transportError
	return errors.As(err, &transport)
}

// dialError marks a request that never reached PageBroker.
type dialError struct {
	cause error
}

func (e dialError) Error() string { return e.cause.Error() }

func (e dialError) Unwrap() error { return e.cause }

// IsDialError reports whether err came from failing to connect to PageBroker,
// in which case the request never reached it.
func IsDialError(err error) bool {
	var dial dialError
	return errors.As(err, &dial)
}

// failureError carries a Failure reply from PageBroker.
type failureError struct {
	code    Failure_Code
	message string
}

func (e failureError) Error() string {
	return fmt.Sprintf("PageBroker %s: %s", e.code, e.message)
}

func failureCode(code Failure_Code) Failure_Code {
	switch code {
	case Failure_UNSPECIFIED, Failure_INVALID_REQUEST, Failure_TRANSACTION_NOT_FOUND, Failure_TRANSACTION_CONFLICT,
		Failure_INSUFFICIENT_STORAGE, Failure_STORAGE_ERROR, Failure_INTERNAL_ERROR:
		return code
	default:
		return Failure_UNSPECIFIED
	}
}

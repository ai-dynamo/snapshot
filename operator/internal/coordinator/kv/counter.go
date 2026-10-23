// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kv

import (
	"encoding/binary"
	"fmt"
)

// CounterSize is the length of a counter value.
const CounterSize = 8

// EncodeCounter returns the value that holds n as a counter: a signed 64-bit
// integer in exactly CounterSize bytes, big-endian and in two's complement.
// That is the one encoding every backend uses, so a counter read as an opaque
// value decodes the same way everywhere.
func EncodeCounter(n int64) []byte {
	value := make([]byte, CounterSize)
	binary.BigEndian.PutUint64(value, uint64(n))
	return value
}

// DecodeCounter returns the counter a value holds, or ErrNotCounter when the
// value is not one.
func DecodeCounter(value []byte) (int64, error) {
	if len(value) != CounterSize {
		return 0, fmt.Errorf("a value of %d bytes: %w", len(value), ErrNotCounter)
	}
	return int64(binary.BigEndian.Uint64(value)), nil
}

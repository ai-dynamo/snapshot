// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinatorv1alpha1

import (
	"encoding/binary"
	"fmt"
)

// CounterSize is the length of a counter value.
const CounterSize = 8

// EncodeCounter returns the value that holds n as a counter, as
// coordinator.proto specifies: a signed 64-bit integer in exactly CounterSize
// bytes, big-endian, in two's complement. A client uses it to write a counter
// with Insert or CompareAndSwap.
func EncodeCounter(n int64) []byte {
	value := make([]byte, CounterSize)
	binary.BigEndian.PutUint64(value, uint64(n))
	return value
}

// DecodeCounter returns the counter a value holds, such as one Get returned, or
// an error when the value is not a counter.
func DecodeCounter(value []byte) (int64, error) {
	if len(value) != CounterSize {
		return 0, fmt.Errorf("a value of %d bytes is not a %d-byte counter", len(value), CounterSize)
	}
	return int64(binary.BigEndian.Uint64(value)), nil
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import "testing"

func TestAllocationStorageRequiresExplicitSupportedOptIn(t *testing.T) {
	for _, test := range []struct {
		storage, shim string
		valid         bool
	}{
		{"", "", true},
		{"host-carrier", "", true},
		{"pagebroker", "enabled", true},
		{"pagebroker", "", false},
		{"future", "enabled", false},
	} {
		annotations := map[string]string{CuinterposeAllocationStorageAnnotation: test.storage}
		if test.shim != "" {
			annotations[CuinterposeAnnotation] = test.shim
		}
		_, err := CuinterposeEnabled(annotations)
		if (err == nil) != test.valid {
			t.Errorf("storage=%q shim=%q: %v", test.storage, test.shim, err)
		}
	}
}

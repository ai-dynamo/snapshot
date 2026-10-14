// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package util holds the small helpers shared across the api module, so the
// components that import it describe the same thing the same way.
package util

import "strings"

// unknownValue stands in for a value nobody recorded. One word for the idea
// everywhere, so a reader never has to work out whether "unset" and "n/a" mean
// the same thing.
const unknownValue = "unknown"

// OrUnknown renders a value that may be absent. Blank is absent rather than
// empty, so a reason never trails off into a dangling comma.
func OrUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return unknownValue
	}
	return value
}

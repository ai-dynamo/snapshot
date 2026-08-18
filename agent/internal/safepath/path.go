// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package safepath validates host paths passed to privileged mount operations.
package safepath

import (
	"fmt"
	"path"
	"strings"
)

// ValidateElement requires one portable ASCII path component.
func ValidateElement(label, value string) error {
	if value == "" || value == "." || value == ".." {
		return fmt.Errorf("%s must be a non-traversing path element: %q", label, value)
	}
	for _, char := range value {
		if !isPortablePathChar(char) {
			return fmt.Errorf("%s contains unsupported character %q: %q", label, char, value)
		}
	}
	return nil
}

// ValidateAbsolute requires a clean absolute path composed of portable ASCII
// elements. The filesystem root is never a safe mount-source boundary.
func ValidateAbsolute(label, value string) error {
	if value == "" || value[0] != '/' || value == "/" || path.Clean(value) != value {
		return fmt.Errorf("%s must be a non-root absolute clean path: %q", label, value)
	}
	for _, element := range strings.Split(value[1:], "/") {
		if err := ValidateElement(label+" component", element); err != nil {
			return err
		}
	}
	return nil
}

// ValidateWithin requires source to equal root or be a descendant separated by
// a path-component boundary.
func ValidateWithin(label, root, source string) error {
	if err := ValidateAbsolute("allowed source root", root); err != nil {
		return err
	}
	if err := ValidateAbsolute(label, source); err != nil {
		return err
	}
	if source != root && !strings.HasPrefix(source, root+"/") {
		return fmt.Errorf("%s %q must be within allowed source root %q", label, source, root)
	}
	return nil
}

func isPortablePathChar(char rune) bool {
	return char >= 'a' && char <= 'z' ||
		char >= 'A' && char <= 'Z' ||
		char >= '0' && char <= '9' ||
		char == '_' || char == '-' || char == '.'
}

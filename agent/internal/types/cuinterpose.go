// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

const (
	CuInterposeFrontend = "libcuinterpose.so"
	CuInterposeCore     = "libcuinterpose_core.so"
)

// CuInterposeLibraries lists every shim library whose hash the manifest records.
var CuInterposeLibraries = []string{CuInterposeFrontend, CuInterposeCore}

// CuInterposeLibraryIdentity identifies a delivered shim library.
type CuInterposeLibraryIdentity struct {
	SHA256 string `yaml:"sha256"`
}

// CuInterposeManifest records the shim libraries a checkpoint was captured with
// and the processes whose core serves a coordinator endpoint.
type CuInterposeManifest struct {
	// Libraries maps each delivered shim library file name to its identity.
	Libraries map[string]CuInterposeLibraryIdentity `yaml:"libraries"`
	// PIDs are the innermost namespace PIDs whose core serves a coordinator endpoint.
	// An explicit empty list means CUDA processes loaded only the frontend.
	PIDs []int `yaml:"pids"`
}

// Validate checks the library hashes and the PID list.
func (m *CuInterposeManifest) Validate() error {
	if len(m.Libraries) != len(CuInterposeLibraries) {
		return fmt.Errorf("cuinterpose.libraries must contain exactly %v; recreate the checkpoint", CuInterposeLibraries)
	}
	for _, library := range CuInterposeLibraries {
		digest, err := hex.DecodeString(m.Libraries[library].SHA256)
		if err != nil || len(digest) != sha256.Size {
			return fmt.Errorf("cuinterpose.libraries[%s].sha256 must contain a SHA-256 hash; recreate the checkpoint", library)
		}
	}
	if m.PIDs == nil {
		return fmt.Errorf("cuinterpose.pids must be present, including an explicit empty list; recreate the checkpoint")
	}
	seen := make(map[int]bool, len(m.PIDs))
	for _, pid := range m.PIDs {
		if pid <= 0 || seen[pid] {
			return fmt.Errorf("cuinterpose.pids must contain positive, unique namespace PIDs")
		}
		seen[pid] = true
	}
	return nil
}

// UsesCoordinator reports whether any process initialized the core and therefore
// serves a coordinator endpoint. Library identity and restore delivery apply even
// when only the frontend was loaded.
func (m *CuInterposeManifest) UsesCoordinator() bool {
	return m != nil && len(m.PIDs) > 0
}

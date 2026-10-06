// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"

	"gopkg.in/yaml.v3"
)

type CuInterposeManifest struct {
	FrontendSHA256 string `yaml:"frontendSHA256"`
	CoreSHA256     string `yaml:"coreSHA256"`
	// PIDs are the namespace PIDs whose core runtime participates in the coordinator.
	// An explicit empty list means only the frontend was loaded in CUDA processes.
	PIDs []int `yaml:"pids"`
}

func (m *CuInterposeManifest) Validate() error {
	if m == nil {
		return nil
	}
	for _, field := range []struct{ name, hash string }{
		{"frontendSHA256", m.FrontendSHA256},
		{"coreSHA256", m.CoreSHA256},
	} {
		digest, err := hex.DecodeString(field.hash)
		if err != nil || len(digest) != sha256.Size {
			return fmt.Errorf("cuinterpose.%s must contain a SHA-256 hash; recreate the checkpoint", field.name)
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

func (m *CuInterposeManifest) ValidateCUDAPIDs(cudaPIDs []int) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if m != nil {
		for _, pid := range m.PIDs {
			if !slices.Contains(cudaPIDs, pid) {
				return fmt.Errorf("cuinterpose PID %d is not a CUDA participant", pid)
			}
		}
	}
	return nil
}

// HasRuntime reports whether coordinator participation is required. Library identity
// and restore delivery still apply when only the frontend was loaded.
func (m *CuInterposeManifest) HasRuntime() bool {
	return m != nil && len(m.PIDs) > 0
}

func (m *CuInterposeManifest) UnmarshalYAML(node *yaml.Node) error {
	type manifest CuInterposeManifest
	if err := node.Decode((*manifest)(m)); err != nil {
		return err
	}
	return m.Validate()
}

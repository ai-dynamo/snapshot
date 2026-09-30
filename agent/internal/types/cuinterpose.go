// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"gopkg.in/yaml.v3"
)

type CuInterposeManifest struct {
	FrontendSHA256 string `yaml:"frontendSHA256"`
	CoreSHA256     string `yaml:"coreSHA256"`
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
	return nil
}

func (m *CuInterposeManifest) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("obsolete cuinterpose manifest: recreate the checkpoint to record library hashes")
	}
	type manifest CuInterposeManifest
	if err := node.Decode((*manifest)(m)); err != nil {
		return err
	}
	return m.Validate()
}

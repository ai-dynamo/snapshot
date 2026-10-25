// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/util/validation"
)

// PVCRoot is the base path inside the claim currently used by Snapshot. It is
// independent of the container-local /checkpoints mount point.
const PVCRoot = "/"

// PVC identifies a logical store inside a namespaced PVC. BasePath is absolute
// within the claim, not the mount path in a consuming container. A claim name
// identifies a configured location, not a particular PVC/PV incarnation.
type PVC struct {
	Namespace string `json:"namespace" yaml:"namespace"`
	ClaimName string `json:"claimName" yaml:"claimName"`
	BasePath  string `json:"basePath" yaml:"basePath"`
}

// StoreID hashes the canonical PVC location. No filesystem or Kubernetes lookup
// is performed; credentials, node names and container mount paths are not inputs.
// The exact v1 preimage is five length-prefixed UTF-8 fields, in order:
// "snapshot.store/v1", "pvc", namespace, claimName, normalizedBasePath.
// Each length is an unsigned 32-bit big-endian byte count (no terminator).
// Shared vectors in testdata/identity.json pin this encoding for other languages.
func (p PVC) StoreID() (string, error) {
	if errs := validation.IsDNS1123Label(p.Namespace); len(errs) != 0 {
		return "", fmt.Errorf("PVC namespace: %s", strings.Join(errs, "; "))
	}
	if errs := validation.IsDNS1123Subdomain(p.ClaimName); len(errs) != 0 {
		return "", fmt.Errorf("PVC claim name: %s", strings.Join(errs, "; "))
	}
	base, err := p.NormalizedBasePath()
	if err != nil {
		return "", err
	}
	return "store-v1-" + hashFields("snapshot.store/v1", "pvc", p.Namespace, p.ClaimName, base), nil
}

// NormalizedBasePath cleans redundant separators and dot components, but rejects
// traversal instead of silently identifying a different store. An empty path is
// not an implicit root; configuration must state which location it identifies.
func (p PVC) NormalizedBasePath() (string, error) {
	base := p.BasePath
	if !utf8.ValidString(base) || len(base) > 4096 || !path.IsAbs(base) ||
		strings.TrimSpace(base) != base || strings.Contains(base, `\`) ||
		strings.ContainsFunc(base, unicode.IsControl) {
		return "", fmt.Errorf("PVC base path must be an absolute path within the claim, at most 4096 bytes, " +
			"without control characters or backslashes")
	}
	for component := range strings.SplitSeq(base, "/") {
		if component == ".." {
			return "", fmt.Errorf("PVC base path must not contain parent traversal")
		}
	}
	return path.Clean(base), nil
}

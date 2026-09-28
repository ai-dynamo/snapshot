// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package storage defines the storage identity shared by Snapshot and PageBroker.
// It does not access a store or select a transfer engine.
package storage

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/util/validation"
)

// PVCRoot is the base path inside the claim currently used by Snapshot. It is
// independent of the container-local /checkpoints mount point.
const PVCRoot = "/"

var storeIDPattern = regexp.MustCompile(`^store-v1-[0-9a-f]{64}$`)

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

// CommitID identifies one container publication of one capture attempt. The
// artifact UID is the owning PodSnapshotContent UID, not its reusable name or an
// RPC transaction ID. Retries/restarts derive the same ID; new content gets a new
// UID. The v1 preimage uses the same framing as StoreID, with fields:
// "snapshot.commit/v1", storeID, artifactUID, containerName.
func CommitID(storeID, artifactUID, containerName string) (string, error) {
	if !storeIDPattern.MatchString(storeID) {
		return "", fmt.Errorf("store ID must be store-v1- followed by 64 lowercase hexadecimal characters")
	}
	// Kubernetes UIDs are opaque. Bound the encoding without assuming UUID syntax.
	if artifactUID == "" || len(artifactUID) > 256 || !utf8.ValidString(artifactUID) ||
		strings.ContainsAny(artifactUID, `/\`) || strings.ContainsFunc(artifactUID, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) {
		return "", fmt.Errorf("artifact UID must be nonempty, at most 256 bytes, " +
			"without whitespace, control characters or path separators")
	}
	if errs := validation.IsDNS1123Label(containerName); len(errs) != 0 {
		return "", fmt.Errorf("container name: %s", strings.Join(errs, "; "))
	}
	return "commit-v1-" + hashFields("snapshot.commit/v1", storeID, artifactUID, containerName), nil
}

func hashFields(fields ...string) string {
	hash := sha256.New()
	for _, field := range fields {
		// Public entry points bound every field well below uint32's limit.
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(field)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(field))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

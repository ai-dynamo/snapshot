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
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/util/validation"
)

var storeIDPattern = regexp.MustCompile(`^store-v1-[0-9a-f]{64}$`)

// ValidateArtifactIdentity checks the three fields that name one container
// publication: the bound store ID, the owning PodSnapshotContent UID (not its
// reusable name or an RPC transaction ID) and the container name.
func ValidateArtifactIdentity(storeID, artifactUID, containerName string) error {
	if !storeIDPattern.MatchString(storeID) {
		return fmt.Errorf("store ID must be store-v1- followed by 64 lowercase hexadecimal characters")
	}
	// Kubernetes UIDs are opaque. Bound the encoding without assuming UUID syntax.
	if artifactUID == "" || len(artifactUID) > 256 || !utf8.ValidString(artifactUID) ||
		strings.ContainsAny(artifactUID, `/\`) || strings.ContainsFunc(artifactUID, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) {
		return fmt.Errorf("artifact UID must be nonempty, at most 256 bytes, " +
			"without whitespace, control characters or path separators")
	}
	if errs := validation.IsDNS1123Label(containerName); len(errs) != 0 {
		return fmt.Errorf("container name: %s", strings.Join(errs, "; "))
	}
	return nil
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

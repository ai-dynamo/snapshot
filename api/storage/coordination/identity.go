// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordination

import (
	"fmt"

	"github.com/ai-dynamo/snapshot/api/storage"
)

// ErrStoreMismatch means the caller's bound store differs from the store a
// descriptor or request names. It is never storage I/O: refuse before
// touching the backend.
var ErrStoreMismatch = fmt.Errorf("storage binding does not match the configured store")

// ValidateBinding confirms a content's declared store/content/container
// identity is well-formed and derives the deterministic commitID for it. It
// performs no storage or Kubernetes lookup.
func ValidateBinding(storeID, artifactUID, containerName string) (commitID string, err error) {
	commitID, err = storage.CommitID(storeID, artifactUID, containerName)
	if err != nil {
		return "", fmt.Errorf("invalid storage binding: %w", err)
	}
	return commitID, nil
}

// RequireStoreMatch refuses a request or descriptor that names a different
// store than the one a content is bound to. Callers must not fall back to
// another store or silently reinterpret the binding.
func RequireStoreMatch(boundStoreID, namedStoreID string) error {
	if boundStoreID == "" || namedStoreID == "" || boundStoreID != namedStoreID {
		return fmt.Errorf("%w: bound to %q, request named %q", ErrStoreMismatch, boundStoreID, namedStoreID)
	}
	return nil
}

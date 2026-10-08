// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordination

import (
	"fmt"

	"github.com/ai-dynamo/snapshot/api/storage"
)

// ErrStoreMismatch means the bound store differs from the store a request names.
var ErrStoreMismatch = fmt.Errorf("storage binding does not match the configured store")

// ValidateBinding checks identity well-formedness and derives the
// deterministic commitID. No storage or Kubernetes lookup.
func ValidateBinding(storeID, artifactUID, containerName string) (commitID string, err error) {
	commitID, err = storage.CommitID(storeID, artifactUID, containerName)
	if err != nil {
		return "", fmt.Errorf("invalid storage binding: %w", err)
	}
	return commitID, nil
}

// RequireStoreMatch refuses a request naming a different store than the one
// a content is bound to.
func RequireStoreMatch(boundStoreID, namedStoreID string) error {
	if boundStoreID == "" || namedStoreID == "" || boundStoreID != namedStoreID {
		return fmt.Errorf("%w: bound to %q, request named %q", ErrStoreMismatch, boundStoreID, namedStoreID)
	}
	return nil
}

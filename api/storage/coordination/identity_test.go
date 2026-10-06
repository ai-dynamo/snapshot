// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordination

import (
	"errors"
	"testing"

	"github.com/ai-dynamo/snapshot/api/storage"
)

func validStoreID(t *testing.T) string {
	t.Helper()
	id, err := storage.PVC{Namespace: "ns", ClaimName: "claim", BasePath: "/checkpoints"}.StoreID()
	if err != nil {
		t.Fatalf("StoreID() error = %v", err)
	}
	return id
}

func TestValidateBindingDerivesDeterministicCommitID(t *testing.T) {
	storeID := validStoreID(t)
	a, err := ValidateBinding(storeID, "content-uid", "main")
	if err != nil {
		t.Fatalf("ValidateBinding() error = %v", err)
	}
	b, err := ValidateBinding(storeID, "content-uid", "main")
	if err != nil {
		t.Fatalf("ValidateBinding() error = %v", err)
	}
	if a != b {
		t.Fatalf("ValidateBinding() not deterministic: %q != %q", a, b)
	}
	c, err := ValidateBinding(storeID, "other-content-uid", "main")
	if err != nil {
		t.Fatalf("ValidateBinding() error = %v", err)
	}
	if a == c {
		t.Fatal("ValidateBinding() produced the same commitID for different artifact UIDs")
	}
}

func TestValidateBindingRejectsMalformedInput(t *testing.T) {
	if _, err := ValidateBinding("not-a-store-id", "content-uid", "main"); err == nil {
		t.Fatal("ValidateBinding() accepted a malformed store ID")
	}
}

func TestRequireStoreMatch(t *testing.T) {
	storeID := validStoreID(t)
	if err := RequireStoreMatch(storeID, storeID); err != nil {
		t.Fatalf("RequireStoreMatch() matching IDs error = %v", err)
	}
	err := RequireStoreMatch(storeID, "store-v1-other")
	if !errors.Is(err, ErrStoreMismatch) {
		t.Fatalf("RequireStoreMatch() mismatched IDs error = %v, want ErrStoreMismatch", err)
	}
	if err := RequireStoreMatch("", storeID); !errors.Is(err, ErrStoreMismatch) {
		t.Fatalf("RequireStoreMatch() empty bound ID error = %v, want ErrStoreMismatch", err)
	}
}

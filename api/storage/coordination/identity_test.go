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

func TestValidateBindingAcceptsWellFormedInput(t *testing.T) {
	if err := ValidateBinding(validStoreID(t), "content-uid", "main"); err != nil {
		t.Fatalf("ValidateBinding() error = %v", err)
	}
}

func TestValidateBindingRejectsMalformedInput(t *testing.T) {
	if err := ValidateBinding("not-a-store-id", "content-uid", "main"); err == nil {
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

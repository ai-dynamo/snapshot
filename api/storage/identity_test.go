// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/ai-dynamo/snapshot/api/storage"
)

// These language-neutral vectors pin the byte encoding as well as the digest.
// External PageBroker implementations must exercise the same file in C++.
func TestSharedIdentityVectors(t *testing.T) {
	data, err := os.ReadFile("testdata/identity.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Stores []struct {
			Name         string
			PVC          storage.PVC
			CanonicalHex string
			StoreID      string
		}
	}
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.Stores) == 0 {
		t.Fatal("store vectors are required")
	}
	for _, v := range vectors.Stores {
		t.Run("store/"+v.Name, func(t *testing.T) {
			assertCanonicalDigest(t, v.CanonicalHex, "store-v1-", v.StoreID)
			// Repeated calls do not depend on process state or a random seed.
			for range 2 {
				got, err := v.PVC.StoreID()
				if err != nil || got != v.StoreID {
					t.Fatalf("StoreID() = %q, %v; want %q", got, err, v.StoreID)
				}
			}
		})
	}
}

func assertCanonicalDigest(t *testing.T, canonical, prefix, want string) {
	t.Helper()
	data, err := hex.DecodeString(canonical)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	if got := prefix + hex.EncodeToString(digest[:]); got != want {
		t.Fatalf("canonical encoding hashes to %q, want %q", got, want)
	}
}

func TestPVCStoreIDRejectsInvalidIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*storage.PVC)
	}{
		{"empty namespace", func(p *storage.PVC) { p.Namespace = "" }},
		{"invalid namespace", func(p *storage.PVC) { p.Namespace = "Snapshot" }},
		{"long namespace", func(p *storage.PVC) { p.Namespace = strings.Repeat("a", 64) }},
		{"empty claim", func(p *storage.PVC) { p.ClaimName = "" }},
		{"invalid claim", func(p *storage.PVC) { p.ClaimName = "../claim" }},
		{"long claim", func(p *storage.PVC) { p.ClaimName = strings.Repeat("a", 254) }},
		{"empty path", func(p *storage.PVC) { p.BasePath = "" }},
		{"relative path", func(p *storage.PVC) { p.BasePath = "checkpoints" }},
		{"parent traversal", func(p *storage.PVC) { p.BasePath = "/a/../b" }},
		{"leading space", func(p *storage.PVC) { p.BasePath = " /" }},
		{"trailing space", func(p *storage.PVC) { p.BasePath = "/ " }},
		{"backslash", func(p *storage.PVC) { p.BasePath = `/a\b` }},
		{"control character", func(p *storage.PVC) { p.BasePath = "/a\x00b" }},
		{"invalid UTF-8", func(p *storage.PVC) { p.BasePath = "/\xff" }},
		{"long path", func(p *storage.PVC) { p.BasePath = "/" + strings.Repeat("x", 4096) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pvc := storage.PVC{Namespace: "snapshot", ClaimName: "snapshot-pvc", BasePath: storage.PVCRoot}
			tc.change(&pvc)
			if id, err := pvc.StoreID(); err == nil || id != "" {
				t.Fatalf("StoreID() = %q, %v; want empty ID and error", id, err)
			}
		})
	}
}

func TestNormalizedPVCBasePath(t *testing.T) {
	for raw, want := range map[string]string{
		"/": "/", "///./": "/", "/a//b/./": "/a/b", "/modèles/": "/modèles",
	} {
		t.Run(raw, func(t *testing.T) {
			pvc := storage.PVC{BasePath: raw}
			got, err := pvc.NormalizedBasePath()
			if err != nil || got != want {
				t.Fatalf("NormalizedBasePath() = %q, %v; want %q", got, err, want)
			}
			if pvc.BasePath != raw {
				t.Fatal("normalization mutated its input")
			}
		})
	}
}

func TestValidateArtifactIdentityRejectsInvalidIdentity(t *testing.T) {
	validStore := "store-v1-" + strings.Repeat("a", 64)
	for _, tc := range []struct{ name, store, uid, container string }{
		{"empty store", "", "uid", "main"},
		{"short digest", "store-v1-a", "uid", "main"},
		{"uppercase digest", "store-v1-" + strings.Repeat("A", 64), "uid", "main"},
		{"unknown version", "store-v2-" + strings.Repeat("a", 64), "uid", "main"},
		{"empty UID", validStore, "", "main"},
		{"UID separator", validStore, "a/b", "main"},
		{"UID backslash", validStore, `a\b`, "main"},
		{"UID whitespace", validStore, "a b", "main"},
		{"UID control", validStore, "a\x00b", "main"},
		{"UID UTF-8", validStore, "\xff", "main"},
		{"long UID", validStore, strings.Repeat("a", 257), "main"},
		{"empty container", validStore, "uid", ""},
		{"invalid container", validStore, "uid", "Main"},
		{"long container", validStore, "uid", strings.Repeat("a", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := storage.ValidateArtifactIdentity(tc.store, tc.uid, tc.container); err == nil {
				t.Fatal("ValidateArtifactIdentity() = nil; want error")
			}
		})
	}
	if err := storage.ValidateArtifactIdentity(validStore, "opaque-kubernetes-uid", "main"); err != nil {
		t.Fatalf("UIDs need not use UUID syntax: %v", err)
	}
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage_test

import (
	"strings"
	"testing"

	"github.com/ai-dynamo/snapshot/api/storage"
)

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

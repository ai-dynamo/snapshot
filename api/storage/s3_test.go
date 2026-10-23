// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage_test

import (
	"strings"
	"testing"

	"github.com/ai-dynamo/snapshot/api/storage"
)

func validS3() storage.S3 {
	return storage.S3{Endpoint: "https://s3.example.com", Region: "us-east-1", Bucket: "snapshot-bucket", Prefix: ""}
}

func TestS3StoreIDRejectsInvalidIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*storage.S3)
	}{
		{"empty endpoint", func(s *storage.S3) { s.Endpoint = "" }},
		{"unsupported scheme", func(s *storage.S3) { s.Endpoint = "ftp://s3.example.com" }},
		{"endpoint with path", func(s *storage.S3) { s.Endpoint = "https://s3.example.com/prefix" }},
		{"endpoint with query", func(s *storage.S3) { s.Endpoint = "https://s3.example.com?x=1" }},
		{"endpoint with userinfo", func(s *storage.S3) { s.Endpoint = "https://user@s3.example.com" }},
		{"malformed endpoint", func(s *storage.S3) { s.Endpoint = "://bad" }},
		{"empty region", func(s *storage.S3) { s.Region = "" }},
		{"non-ascii region", func(s *storage.S3) { s.Region = "us-éast-1" }},
		{"short bucket", func(s *storage.S3) { s.Bucket = "ab" }},
		{"uppercase bucket", func(s *storage.S3) { s.Bucket = "Snapshot-Bucket" }},
		{"bucket leading dot", func(s *storage.S3) { s.Bucket = ".snapshot-bucket" }},
		{"bucket trailing hyphen", func(s *storage.S3) { s.Bucket = "snapshot-bucket-" }},
		{"bucket invalid char", func(s *storage.S3) { s.Bucket = "snapshot_bucket" }},
		{"absolute prefix", func(s *storage.S3) { s.Prefix = "/checkpoints" }},
		{"prefix traversal", func(s *storage.S3) { s.Prefix = "a/../b" }},
		{"prefix leading space", func(s *storage.S3) { s.Prefix = " checkpoints" }},
		{"prefix backslash", func(s *storage.S3) { s.Prefix = `a\b` }},
		{"prefix control character", func(s *storage.S3) { s.Prefix = "a\x00b" }},
		{"prefix invalid UTF-8", func(s *storage.S3) { s.Prefix = "\xff" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s3 := validS3()
			tc.change(&s3)
			if id, err := s3.StoreID(); err == nil || id != "" {
				t.Fatalf("StoreID() = %q, %v; want empty ID and error", id, err)
			}
		})
	}
}

func TestS3StoreIDIsStableAndCaseInsensitiveOnEndpoint(t *testing.T) {
	lower := storage.S3{Endpoint: "https://s3.example.com", Region: "us-east-1", Bucket: "snapshot-bucket"}
	upper := storage.S3{Endpoint: "HTTPS://S3.EXAMPLE.COM", Region: "us-east-1", Bucket: "snapshot-bucket"}
	id1, err := lower.StoreID()
	if err != nil {
		t.Fatal(err)
	}
	id2, err := upper.StoreID()
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("expected endpoint case to be ignored: %q != %q", id1, id2)
	}
	id3, err := lower.StoreID()
	if err != nil || id3 != id1 {
		t.Fatalf("StoreID() is not stable across calls: %q, %q, %v", id1, id3, err)
	}
}

func TestS3StoreIDDistinguishesEachField(t *testing.T) {
	base := validS3()
	baseID, err := base.StoreID()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*storage.S3)
	}{
		{"endpoint", func(s *storage.S3) { s.Endpoint = "https://other.example.com" }},
		{"region", func(s *storage.S3) { s.Region = "us-west-2" }},
		{"bucket", func(s *storage.S3) { s.Bucket = "other-bucket" }},
		{"prefix", func(s *storage.S3) { s.Prefix = "containers" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s3 := validS3()
			tc.change(&s3)
			id, err := s3.StoreID()
			if err != nil {
				t.Fatal(err)
			}
			if id == baseID {
				t.Fatalf("changing %s did not change the store ID", tc.name)
			}
		})
	}
}

func TestNormalizedS3Prefix(t *testing.T) {
	for raw, want := range map[string]string{
		"": "", ".": "", "a//b/./": "a/b", "a/b/": "a/b", "modèles": "modèles",
	} {
		t.Run(raw, func(t *testing.T) {
			s3 := storage.S3{Prefix: raw}
			got, err := s3.NormalizedPrefix()
			if err != nil || got != want {
				t.Fatalf("NormalizedPrefix() = %q, %v; want %q", got, err, want)
			}
			if s3.Prefix != raw {
				t.Fatal("normalization mutated its input")
			}
		})
	}
}

// S3 has no PageBroker-side identity computation to cross-check: PageBroker's
// S3 backend reads storeId as a literal config field rather than deriving it.
// There is no cross-language parity vector for it, unlike PVC's
// testdata/identity.json.
func TestS3StoreIDHasStorePrefix(t *testing.T) {
	id, err := validS3().StoreID()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "store-v1-") || len(id) != len("store-v1-")+64 {
		t.Fatalf("StoreID() = %q; want store-v1- followed by 64 hex characters", id)
	}
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordination

import (
	"errors"
	"testing"
)

func TestMatchEvidenceExactMatch(t *testing.T) {
	found := []Evidence{
		{CommitID: "commit-a", ArtifactHandle: "handle-a", FormatVersion: "v1"},
		{CommitID: "commit-b", ArtifactHandle: "handle-b", FormatVersion: "v1"},
	}
	got, err := MatchEvidence("commit-a", found)
	if err != nil {
		t.Fatalf("MatchEvidence() error = %v", err)
	}
	if got != found[0] {
		t.Fatalf("MatchEvidence() = %+v, want %+v", got, found[0])
	}
}

func TestMatchEvidenceNoMatch(t *testing.T) {
	_, err := MatchEvidence("commit-missing", []Evidence{{CommitID: "commit-a"}})
	if !errors.Is(err, ErrNoEvidence) {
		t.Fatalf("MatchEvidence() error = %v, want ErrNoEvidence", err)
	}
}

func TestMatchEvidenceConflict(t *testing.T) {
	found := []Evidence{
		{CommitID: "commit-a", ArtifactHandle: "handle-1", FormatVersion: "v1"},
		{CommitID: "commit-a", ArtifactHandle: "handle-2", FormatVersion: "v1"},
	}
	_, err := MatchEvidence("commit-a", found)
	if !errors.Is(err, ErrConflictingEvidence) {
		t.Fatalf("MatchEvidence() error = %v, want ErrConflictingEvidence", err)
	}
}

func TestMatchEvidenceDuplicateIsNotConflict(t *testing.T) {
	found := []Evidence{
		{CommitID: "commit-a", ArtifactHandle: "handle-1", FormatVersion: "v1"},
		{CommitID: "commit-a", ArtifactHandle: "handle-1", FormatVersion: "v1"},
	}
	got, err := MatchEvidence("commit-a", found)
	if err != nil {
		t.Fatalf("MatchEvidence() error = %v", err)
	}
	if got != found[0] {
		t.Fatalf("MatchEvidence() = %+v, want %+v", got, found[0])
	}
}

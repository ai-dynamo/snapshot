// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordination

import "errors"

// Evidence is one discovered publication at the storage backend. CommitID is
// the deterministic identity from storage.CommitID, never a write timestamp;
// recovery must never select evidence by when it was written.
type Evidence struct {
	CommitID       string
	ArtifactHandle string
	FormatVersion  string
}

// ErrNoEvidence means nothing at the backend matches the expected commitID:
// a legitimate "not published yet" or "already deleted," not a conflict.
var ErrNoEvidence = errors.New("no matching publication evidence")

// ErrConflictingEvidence means more than one distinct publication claims the
// same commitID. Repair must refuse explicitly rather than guess.
var ErrConflictingEvidence = errors.New("conflicting publication evidence")

// MatchEvidence finds the exact publication for expectedCommitID among
// discovered evidence. Multiple entries with the same commitID are only a
// conflict if they disagree on handle or format version; backends may
// legitimately report the same confirmed publication more than once.
func MatchEvidence(expectedCommitID string, found []Evidence) (Evidence, error) {
	var match *Evidence
	for i := range found {
		if found[i].CommitID != expectedCommitID {
			continue
		}
		if match == nil {
			match = &found[i]
			continue
		}
		if match.ArtifactHandle != found[i].ArtifactHandle || match.FormatVersion != found[i].FormatVersion {
			return Evidence{}, ErrConflictingEvidence
		}
	}
	if match == nil {
		return Evidence{}, ErrNoEvidence
	}
	return *match, nil
}

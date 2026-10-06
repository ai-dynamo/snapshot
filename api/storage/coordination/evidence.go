// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordination

import "errors"

// Evidence is one discovered publication, keyed by storage.CommitID — never
// selected by write timestamp.
type Evidence struct {
	CommitID       string
	ArtifactHandle string
	FormatVersion  string
}

// ErrNoEvidence means nothing matches the expected commitID.
var ErrNoEvidence = errors.New("no matching publication evidence")

// ErrConflictingEvidence means two distinct publications claim the same commitID.
var ErrConflictingEvidence = errors.New("conflicting publication evidence")

// MatchEvidence finds the exact publication for expectedCommitID. Duplicate
// entries for the same commitID are fine; disagreeing ones are a conflict.
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

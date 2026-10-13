// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <stdexcept>
#include <string>

#include "pagebroker_types.hpp"
#include "transfer_engine.hpp"

namespace snapshot::pagebroker {

// ArtifactError carries the typed Failure::Code an artifact-addressed
// operation failed with, so broker.cpp can return it directly instead of
// collapsing every artifact failure into STORAGE_ERROR the way the generic
// catch in Broker::HandleRequest does for std::exception.
class ArtifactError : public std::runtime_error {
 public:
  ArtifactError(Failure::Code code, const std::string& message) : std::runtime_error(message), code_(code) {}
  Failure::Code code() const { return code_; }

 private:
  Failure::Code code_;
};

// RestorePlan is a verified, resolved location for one publication's payload.
// Resolving one proves the publication exists and its handle names this
// store's layout; it does not yet mean the payload is staged.
struct RestorePlan {
  Path container_directory;
};

// PVCArtifactStore implements artifact-addressed checkpoint and restore over
// the existing artifacts/<contentUID>/containers/<name>/ layout the legacy
// filesystem path already publishes into — not a new on-disk scheme. A PVC
// publication has no index: the directory itself is the publication and its
// relative path is the opaque-to-callers handle, matching
// agent/internal/pagebroker/filesystem_artifact.go's FilesystemArtifact.
class PVCArtifactStore {
 public:
  // store_id is this installation's configured store (ComputeStoreID of its
  // storage.yaml), used to refuse any ArtifactTarget/PublishedArtifact naming
  // a different store before touching storage.
  PVCArtifactStore(std::string store_id, Path storage_root);

  const std::string& store_id() const { return store_id_; }

  // Validates target against the configured store and the shared artifact
  // identity rules, requires the store root to be reachable, and returns the
  // fixed directory the checkpoint will publish into. The fixed path holds
  // exactly one publication, so a path already published or still being
  // published is refused here, before any capture runs. Does not create it;
  // PublishCheckpoint does, atomically. Throws ArtifactError (STORE_MISMATCH,
  // INVALID_REQUEST, STORAGE_UNAVAILABLE, ACCESS_DENIED, TRANSACTION_CONFLICT).
  Path ValidateTarget(const ArtifactTarget& target) const;

  // Atomically publishes staging_directory's contents (already containing
  // the agent-written manifest.yaml and checkpoint payload) at the fixed
  // path for target and fills *out with the resulting descriptor.
  // staging_directory is copied, not consumed: the caller removes it
  // afterward. Re-runs ValidateTarget, so an occupied path still fails with
  // ArtifactError(TRANSACTION_CONFLICT) at Commit and is left untouched. The
  // partial is created exclusively and the final rename never replaces, so a
  // concurrent publication of the same target fails closed as well.
  void PublishCheckpoint(const Path& staging_directory, const ArtifactTarget& target, PublishedArtifact* out) const;

  // Resolves and verifies a previously published artifact: store match,
  // handle within this store's layout, store root reachable, publication
  // present with its manifest. Throws ArtifactError with the matching typed
  // code (STORE_MISMATCH, ARTIFACT_CORRUPT, STORAGE_UNAVAILABLE,
  // ACCESS_DENIED, ARTIFACT_NOT_FOUND) — an access error never proves
  // absence, and it never guesses by substituting another publication.
  RestorePlan ResolveRestorePlan(const PublishedArtifact& artifact) const;

  // Copies only manifest.yaml from the resolved directory into destination,
  // for GetArtifactMetadata.
  void StageMetadata(const RestorePlan& plan, const Path& destination) const;

  // Copies CPU images into destination for CRIU restore; GPU payload stays
  // in the published directory for the GPU engine to read directly.
  void StageRestore(const RestorePlan& plan, const Path& destination) const;

 private:
  void RequireStorageRoot() const;

  std::string store_id_;
  Path storage_root_;
};

}  // namespace snapshot::pagebroker

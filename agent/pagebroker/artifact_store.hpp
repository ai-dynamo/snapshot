// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <stdexcept>
#include <string>

#include "pagebroker_types.hpp"
#include "transfer_engine.hpp"

namespace snapshot::pagebroker {

// FilesystemFormatVersion is the format this store writes and the only one it
// reads. Must match agent/internal/pagebroker/filesystem_artifact.go's
// FilesystemFormatVersion exactly; the two sides agree on the value, not on a
// shared source of truth for it.
inline constexpr char kFilesystemFormatVersion[] = "snapshot.pagebroker/v1";

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
// Resolving one proves the publication exists and its evidence matches the
// handle the caller presented; it does not yet mean the payload is staged.
struct RestorePlan {
  Path container_directory;
};

// PVCArtifactStore implements artifact-addressed checkpoint and restore over
// the existing artifacts/<contentUID>/containers/<name>/ layout the legacy
// filesystem path already publishes into — not a new on-disk scheme. The
// artifact handle it returns is a logical, opaque-to-callers locator over
// that same layout (exactly the relative path), matching
// agent/internal/pagebroker/filesystem_artifact.go's FilesystemArtifact.
class PVCArtifactStore {
 public:
  // store_id is this installation's configured store (ComputeStoreID of its
  // storage.yaml), used to refuse any ArtifactTarget/PublishedArtifact naming
  // a different store before touching storage.
  PVCArtifactStore(std::string store_id, Path storage_root);

  const std::string& store_id() const { return store_id_; }

  // Validates target against the configured store and safe path components,
  // and returns the directory the checkpoint will publish into. Does not
  // create it; PublishCheckpoint does, atomically. Throws ArtifactError.
  Path ValidateTarget(const ArtifactTarget& target) const;

  // Atomically publishes staging_directory's contents (already containing
  // the agent-written manifest.yaml and checkpoint payload) into the
  // artifact layout and writes publication evidence (store ID, artifact
  // handle, deterministic commitID) alongside it. Fills *out with the
  // resulting descriptor. staging_directory is consumed: on success it no
  // longer exists at its original path.
  void PublishCheckpoint(const Path& staging_directory, const ArtifactTarget& target, PublishedArtifact* out) const;

  // Resolves and verifies a previously published artifact: store match,
  // publication exists, evidence readable and self-consistent, format
  // version supported. Throws ArtifactError with the matching typed code
  // (STORE_MISMATCH, ARTIFACT_NOT_FOUND, ARTIFACT_CORRUPT,
  // UNSUPPORTED_ARTIFACT) — never guesses by substituting another
  // publication.
  RestorePlan ResolveRestorePlan(const PublishedArtifact& artifact) const;

  // Copies only manifest.yaml from the resolved directory into destination,
  // for GetArtifactMetadata. Throws ArtifactError(ARTIFACT_CORRUPT) if it is
  // missing.
  void StageMetadata(const RestorePlan& plan, const Path& destination) const;

  // Copies CPU images into destination for CRIU restore; GPU payload stays
  // in the published directory for the GPU engine to read directly.
  void StageRestore(const RestorePlan& plan, const Path& destination) const;

 private:
  std::string store_id_;
  Path storage_root_;
};

}  // namespace snapshot::pagebroker

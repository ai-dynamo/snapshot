// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <memory>
#include <mutex>
#include <string>
#include <vector>

#include "checkpoint_index.hpp"
#include "s3_config.hpp"
#include "transfer/restore_plan.hpp"

namespace snapshot::pagebroker {
struct CheckpointPublication;

// Plans and publishes artifacts in one S3 store; transfers require local staging.
class S3StorageBackend {
 public:
  // Validates and retains startup settings; the SDK client is created on demand.
  explicit S3StorageBackend(S3Config config);
  // Keeps the lazy client and its shared resource limits owned by this backend.
  S3StorageBackend(const S3StorageBackend&) = delete;
  S3StorageBackend& operator=(const S3StorageBackend&) = delete;

  // Immutable snapshot of the startup settings supplied by the engine.
  const S3Config& config() const;
  // Derives a stable handle from the target's store, artifact UID and container
  // identity, rejecting targets for another configured store without network I/O.
  PublishedArtifact ResolveTarget(const ArtifactTarget& target) const;
  // Checks descriptor identity and handle syntax against this store.
  void ValidateCheckpoint(const PublishedArtifact& storage) const;
  // Checks whether a bounded index object exists; does not validate its contents.
  bool CheckpointExists(const PublishedArtifact& storage, TransferControl control) const;
  // Rejects an already published index. This preflight does not reserve the target;
  // callers must retain exclusive ownership through payload upload and publication.
  void PrepareCheckpoint(const PublishedArtifact& storage, TransferControl control) const;
  // Selects local capture staging after rejecting direct mode and destination conflicts.
  Path PrepareCheckpoint(const PublishedArtifact& storage, const Path& staging_directory,
      TransferMode mode, TransferControl control) const;

  // Loads an artifact plan for staging, rejecting direct mode before remote reads.
  PreparedRestore PrepareRestore(const PublishedArtifact& storage, bool metadata_only,
      TransferMode mode, TransferControl control) const;

  // Publishes payloads and the final index while retaining uncertain retry state.
  void PublishCheckpoint(const Path& source, RestorePlan plan, CheckpointPublication& publication,
      TransferMode mode, TransferControl control) const;

  // Validates the local tree and index/part limits before remote writes. Assigns
  // payload locations for the inspected tree.
  RestorePlan InspectCheckpoint(const Path& source, const PublishedArtifact& storage, TransferControl control) const;
  // Parses the index and checks payload sizes before assigning Model Streamer
  // source URLs. metadata_only selects manifest.yaml with private staging modes.
  RestorePlan LoadRestorePlan(const PublishedArtifact& storage, bool metadata_only, TransferControl control) const;

  // Uploads an inspected plan and serializes its metadata without publishing it.
  // Keep the source immutable during upload. Save the returned index in the
  // checkpoint publication state before calling PublishIndex, including on retries.
  std::string UploadCheckpointPayloads(const Path& source, RestorePlan plan, TransferControl control) const;
  // Publishes the index last using create-only writes. Identical existing bytes
  // confirm a retry; different bytes conflict, and an unconfirmed write reports
  // OUTCOME_UNKNOWN so callers can retry with the exact saved index.
  void PublishIndex(const PublishedArtifact& storage, const std::string& index, TransferControl control) const;


 private:
  // Initializes one shared SDK client safely on the first operation that needs it.
  S3Client& Client() const;
  // Validates the descriptor and places a key beneath its configured artifact prefix.
  S3ObjectLocation Object(const PublishedArtifact& storage, const std::string& key) const;
  // Uploads planned files in turn, checking their sizes.
  void UploadFiles(const Path& source, const RestorePlan& plan, TransferControl control) const;
  // Returns false for an absent index, true for an exact match, and throws on conflict.
  bool ConfirmIndex(const S3ObjectLocation& object, const std::string& index, TransferControl control) const;

  const S3Config config_;
  mutable std::once_flag client_once_;
  mutable std::unique_ptr<S3Client> client_;
};
}  // namespace snapshot::pagebroker

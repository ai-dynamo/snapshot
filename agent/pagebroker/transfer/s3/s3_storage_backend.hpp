// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <memory>
#include <mutex>
#include <string>
#include <vector>

#include "checkpoint_index.hpp"
#include "s3_config.hpp"
#include "transfer/checkpoint_inventory.hpp"

namespace snapshot::pagebroker {
struct CheckpointPublication;

// Plans and publishes artifacts in one S3 store; transfers require local staging.
// Operations require descriptors returned by this backend's ResolveTarget() or
// accepted by its ValidateArtifactDescriptor() at request admission.
class S3StorageBackend {
 public:
  // Validates and retains startup settings; the rclone client is created on demand.
  explicit S3StorageBackend(S3Config config);
  // Keeps the lazy client and its shared resource limits owned by this backend.
  S3StorageBackend(const S3StorageBackend&) = delete;
  S3StorageBackend& operator=(const S3StorageBackend&) = delete;

  // Immutable snapshot of the startup settings supplied by the engine.
  const S3Config& config() const;
  // Builds the content/container directory handle, rejecting unsafe identities
  // and targets for another configured store without network I/O.
  PublishedArtifact ResolveTarget(const ArtifactTarget& target) const;
  // Checks descriptor identity and handle syntax against this store.
  void ValidateArtifactDescriptor(const PublishedArtifact& storage) const;
  // Rejects an already published index. This preflight does not reserve the target;
  // callers must retain exclusive ownership through payload upload and publication.
  void CheckCheckpointDestination(const PublishedArtifact& storage, Cancellation cancellation) const;

  // Loads an artifact inventory for staging, rejecting direct mode before remote reads.
  PreparedRestore PrepareRestore(const PublishedArtifact& storage, bool metadata_only,
      TransferMode mode, Cancellation cancellation) const;

  // Publishes payloads and the final index while retaining uncertain retry state.
  void PublishCheckpoint(const Path& source, CheckpointInventory inventory, CheckpointPublication& publication,
      Cancellation cancellation) const;

  // Validates the local tree, payload keys and index/part limits before remote writes.
  CheckpointInventory BuildCheckpointInventory(const Path& source, const PublishedArtifact& storage, Cancellation cancellation) const;

 private:
  // Parses the index and validates payload keys before assigning Model Streamer
  // source URLs. metadata_only selects manifest.yaml with private staging modes.
  CheckpointInventory LoadCheckpointInventory(const PublishedArtifact& storage, bool metadata_only, Cancellation cancellation) const;

  // Uploads an inspected inventory and serializes its metadata without publishing it.
  // Keep the source immutable during upload. Save the returned index in the
  // checkpoint publication state before calling PublishIndex, including on retries.
  std::string UploadCheckpointPayloads(const Path& source, const CheckpointInventory& inventory,
      const PublishedArtifact& storage, Cancellation cancellation) const;
  // Publishes the index last using create-only writes. Identical existing bytes
  // confirm a retry; different bytes conflict, and an unconfirmed write reports
  // OUTCOME_UNKNOWN so callers can retry with the exact saved index.
  void PublishIndex(const PublishedArtifact& storage, const std::string& index, Cancellation cancellation) const;

  // Initializes one shared rclone client safely on the first operation that needs it.
  S3Client& Client() const;
  // Places a key beneath the validated artifact's prefix.
  S3ObjectLocation Object(const PublishedArtifact& storage, const std::string& key) const;
  // Uploads the planned files in one batch; overlapping writers are unsupported.
  void UploadFiles(const Path& source, const CheckpointInventory& inventory,
      const PublishedArtifact& storage, Cancellation cancellation) const;
  // Returns false for an absent index, true for an exact match, and throws on conflict.
  bool ConfirmIndex(const S3ObjectLocation& object, const std::string& index, Cancellation cancellation) const;

  const S3Config config_;
  mutable std::once_flag client_once_;
  mutable std::unique_ptr<S3Client> client_;
};
}  // namespace snapshot::pagebroker

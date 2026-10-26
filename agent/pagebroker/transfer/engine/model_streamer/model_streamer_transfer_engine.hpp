// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <memory>
#include <mutex>

#include "s3_config.hpp"
#include "model_streamer_config.hpp"
#include "transfer/s3/s3_storage_backend.hpp"

#include "model_streamer_restore.hpp"
#include "transfer/engine/transfer_engine.hpp"

namespace snapshot::pagebroker {
// Artifact operations require descriptors resolved or validated by this engine
// at request admission through ResolveArtifactTarget() or ValidateArtifact().
class ModelStreamerTransferEngine final : public TransferEngine {
 public:
  // Binds artifact operations to the configured S3 store.
  explicit ModelStreamerTransferEngine(S3Config config, model_streamer::settings::Options model_streamer = {});

  // Identifies the Model Streamer transfer implementation.
  IoEngine type() const override;

  // Loads and validates the published index and payload sizes for staging.
  PreparedRestore PrepareRestore(const StorageBackend& source, Cancellation cancellation = {},
      const PublishedArtifact* artifact = nullptr, bool metadata_only = false,
      TransferMode mode = TransferMode::Staged) const override;

  // Streams prepared S3 objects into owned local staging.
  // Requires preparation from this engine's PrepareRestore.
  RestoreAccess StageRestore(PreparedRestore preparation, const Path& destination,
      Cancellation cancellation = {}) const override;

  // Selects local capture for a staged S3 checkpoint.
  Path PrepareCheckpoint(const StorageBackend& destination, const Path& staging_directory,
      const std::string& transaction_id, TransferMode mode = TransferMode::Staged,
      const PublishedArtifact* artifact = nullptr, Cancellation cancellation = {}) const override;

  // Scans local checkpoint output before publication admission.
  CheckpointInventory InspectCheckpoint(const Path& source, const PublishedArtifact* artifact = nullptr,
      Cancellation cancellation = {}) const override;

  // Delegates checkpoint publication and retry state to the storage backend.
  // Requires a staged checkpoint accepted by PrepareCheckpoint.
  void PublishCheckpoint(const Path& source, const StorageBackend& destination, CheckpointInventory inventory,
      CheckpointPublication* publication = nullptr, Cancellation cancellation = {},
      TransferMode mode = TransferMode::Staged) const override;

  // Resolves a logical artifact target in the configured S3 store.
  PublishedArtifact ResolveArtifactTarget(const ArtifactTarget& target) const override;

  // Validates the store identity and handle of an S3 publication descriptor.
  void ValidateArtifact(const PublishedArtifact& artifact) const override;

 private:
  // Checks restore settings at startup; process-level settings must remain fixed.

  // Creates a coordinator for native restore submissions.
  std::shared_ptr<ModelStreamerRestore> CreateRestore() const;

  // Reuses a healthy coordinator or replaces a failed native session.
  std::shared_ptr<ModelStreamerRestore> AcquireRestore() const;
  S3StorageBackend store_;
  const model_streamer::settings::Options model_streamer_;
  mutable std::mutex restore_mutex_;
  mutable std::shared_ptr<ModelStreamerRestore> restore_;
};
}  // namespace snapshot::pagebroker

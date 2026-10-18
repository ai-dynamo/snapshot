// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <memory>
#include <mutex>
#include <optional>

#include "s3_config.hpp"
#include "model_streamer_config.hpp"
#include "transfer/s3/s3_storage_backend.hpp"

#include "model_streamer_restore.hpp"
#include "transfer/engine/transfer_engine.hpp"

namespace snapshot::pagebroker {
class ModelStreamerTransferEngine final : public TransferEngine {
 public:
  // Retains the storage root used to validate filesystem locations.
  explicit ModelStreamerTransferEngine(Path storage_root, ModelStreamerOptions model_streamer = {});

  // Configures native transfers with explicit S3 connection settings.
  ModelStreamerTransferEngine(Path storage_root, S3TransferOptions options, ModelStreamerOptions model_streamer = {});

  // Binds artifact operations to the configured S3 store.
  explicit ModelStreamerTransferEngine(S3Config config, ModelStreamerOptions model_streamer = {});

  // Identifies the Model Streamer transfer implementation.
  IoEngine type() const override;

  // Delegates source validation and planning to the configured storage backend.
  PreparedRestore PrepareRestore(const StorageBackend& source, Cancellation control = {},
      const PublishedArtifact* artifact = nullptr, bool metadata_only = false,
      TransferMode mode = TransferMode::Staged) const override;

  // Returns direct access or streams the prepared files into owned staging.
  RestoreAccess StageRestore(PreparedRestore preparation, const Path& destination,
      Cancellation control = {}) const override;

  // Selects a local capture directory appropriate for the requested mode.
  Path PrepareCheckpoint(const StorageBackend& destination, const Path& staging_directory,
      const std::string& transaction_id, TransferMode mode = TransferMode::Staged,
      const PublishedArtifact* artifact = nullptr, Cancellation control = {}) const override;

  // Validates the destination through the configured storage backend.
  void ValidateCheckpointDestination(const StorageBackend& destination,
      const PublishedArtifact* artifact = nullptr, Cancellation control = {}) const override;

  // Scans local checkpoint output before publication admission.
  RestorePlan InspectCheckpoint(const Path& source, const PublishedArtifact* artifact = nullptr,
      Cancellation control = {}) const override;

  // Delegates checkpoint publication and retry state to the storage backend.
  void PublishCheckpoint(const Path& source, const StorageBackend& destination, RestorePlan plan,
      CheckpointPublication* publication = nullptr, Cancellation control = {},
      TransferMode mode = TransferMode::Staged) const override;

  // Resolves a logical artifact target in the configured S3 store.
  PublishedArtifact ResolveArtifactTarget(const ArtifactTarget& target) const override;

  // Validates the store identity and handle of an S3 publication descriptor.
  void ValidateArtifact(const PublishedArtifact& artifact) const override;

 private:
  // Checks that upload and restore use consistent S3 connection settings.
  void ValidateS3Configuration() const;

  // Returns the configured artifact backend after validating its settings.
  const S3StorageBackend& ArtifactStorage() const;

  // Creates a coordinator for native restore submissions.
  std::shared_ptr<ModelStreamerRestore> CreateRestore() const;

  // Reuses a healthy coordinator or replaces a failed native session.
  std::shared_ptr<ModelStreamerRestore> AcquireRestore() const;
  Path storage_root_;
  const std::optional<S3TransferOptions> s3_options_;
  const ModelStreamerOptions model_streamer_;
  std::unique_ptr<S3StorageBackend> store_;
  mutable std::mutex restore_mutex_;
  mutable std::shared_ptr<ModelStreamerRestore> restore_;
};
}  // namespace snapshot::pagebroker

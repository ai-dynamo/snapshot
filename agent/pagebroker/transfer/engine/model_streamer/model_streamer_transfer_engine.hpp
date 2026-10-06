// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <memory>
#include <mutex>
#include <optional>

#include "model_streamer_restore.hpp"
#include "model_streamer_config.hpp"
#include "transfer/s3/s3_storage_backend.hpp"
#include "transfer/engine/transfer_engine.hpp"

namespace snapshot::pagebroker {
class ModelStreamerTransferEngine final : public TransferEngine {
 public:
  explicit ModelStreamerTransferEngine(Path storage_root, ModelStreamerOptions model_streamer = {});
  ModelStreamerTransferEngine(Path storage_root, S3TransferOptions options, ModelStreamerOptions model_streamer = {});
  explicit ModelStreamerTransferEngine(S3Config config, ModelStreamerOptions model_streamer = {});
  TransferEngineType type() const override;
  RestorePlan PrepareRestore(const StorageBackend& source, TransferControl control = {},
      const PublishedArtifact* artifact = nullptr, bool metadata_only = false) const override;
  void StageRestore(const RestorePlan& plan, const Path& destination, TransferControl control = {}) const override;
  void ValidateCheckpointDestination(const StorageBackend& destination,
      const PublishedArtifact* artifact = nullptr, TransferControl control = {}) const override;
  void PublishCheckpoint(const Path& source, const StorageBackend& destination, RestorePlan plan,
      CheckpointPublication* publication = nullptr, TransferControl control = {}) const override;
  RestorePlan InspectCheckpoint(const Path& source, const PublishedArtifact* artifact = nullptr,
      TransferControl control = {}) const override;
  PublishedArtifact ResolveArtifactTarget(const ArtifactTarget& target) const override;
  void ValidateArtifact(const PublishedArtifact& artifact) const override;

 private:
  // Storage backends prepare plans; native execution and session recovery are shared.
  void ValidateS3Configuration() const;
  const S3StorageBackend& ArtifactStorage() const;
  std::shared_ptr<ModelStreamerRestore> CreateRestore() const;
  // Returns the current restore session, replacing a terminally failed one.
  std::shared_ptr<ModelStreamerRestore> AcquireRestore() const;

  Path storage_root_;
  const std::optional<S3TransferOptions> s3_options_;
  const ModelStreamerOptions model_streamer_;
  std::unique_ptr<S3StorageBackend> store_;
  mutable std::mutex restore_mutex_;
  mutable std::shared_ptr<ModelStreamerRestore> restore_;
};
}  // namespace snapshot::pagebroker

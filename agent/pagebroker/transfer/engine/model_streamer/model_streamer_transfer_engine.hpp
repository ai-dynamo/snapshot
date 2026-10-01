// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <memory>
#include <mutex>
#include <optional>

#include "model_streamer_restore.hpp"
#include "s3_config.hpp"
#include "transfer/engine/transfer_engine.hpp"

namespace snapshot::pagebroker {
class ModelStreamerTransferEngine final : public TransferEngine {
 public:
  explicit ModelStreamerTransferEngine(Path storage_root);
  ModelStreamerTransferEngine(Path storage_root, S3TransferOptions options);
  TransferEngineType type() const override;
  RestorePlan PrepareRestore(const StorageBackend& source, TransferControl control = {}) const override;
  void StageRestore(const RestorePlan& plan, const Path& destination, TransferControl control = {}) const override;
  void ValidateCheckpointDestination(const StorageBackend& destination) const override;
  bool CheckpointDestinationConflicts(const StorageBackend& destination) const override;
  void PublishCheckpoint(const Path& source, const StorageBackend& destination) const override;

 private:
  // Storage backends prepare plans; native execution and session recovery are shared.
  void ValidateS3Configuration() const;
  std::shared_ptr<ModelStreamerRestore> CreateRestore() const;
  // Returns the current restore session, replacing a terminally failed one.
  std::shared_ptr<ModelStreamerRestore> AcquireRestore() const;

  Path storage_root_;
  const std::optional<S3TransferOptions> s3_options_;
  mutable std::mutex restore_mutex_;
  mutable std::shared_ptr<ModelStreamerRestore> restore_;
};
}  // namespace snapshot::pagebroker

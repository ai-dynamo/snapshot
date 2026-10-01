// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include "transfer/engine/transfer_engine.hpp"

namespace snapshot::pagebroker {
class PosixCopyEngine final : public TransferEngine {
 public:
  explicit PosixCopyEngine(Path storage_root);
  TransferEngineType type() const override;
  RestorePlan PrepareRestore(const StorageBackend& source, TransferControl control = {},
      const PublishedArtifact* artifact = nullptr, bool metadata_only = false) const override;
  void StageRestore(const RestorePlan& plan, const Path& destination, TransferControl control = {}) const override;
  void ValidateCheckpointDestination(const StorageBackend& destination,
      const PublishedArtifact* artifact = nullptr, TransferControl control = {}) const override;
  void PublishCheckpoint(const Path& source, const StorageBackend& destination, RestorePlan plan,
      CheckpointPublication* publication = nullptr, TransferControl control = {}) const override;

 private:
  Path storage_root_;
};
}  // namespace snapshot::pagebroker

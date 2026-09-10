// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <memory>
#include <mutex>

#include "model_streamer_restore.hpp"
#include "posix_copy_engine.hpp"
#include "transfer_engine.hpp"

namespace snapshot::pagebroker {
class ModelStreamerTransferEngine final : public TransferEngine {
 public:
  explicit ModelStreamerTransferEngine(Path storage_root);
  TransferEngineType type() const override;
  uintmax_t RestoreSize(const StorageBackend& source) const override;
  void StageRestore(const StorageBackend& source, const Path& destination) const override;
  void ValidateCheckpointDestination(const StorageBackend& destination) const override;
  bool CheckpointDestinationConflicts(const StorageBackend& destination) const override;
  void PublishCheckpoint(const Path& source, const StorageBackend& destination) const override;

 private:
  // Returns the current restore session, replacing a terminally failed one.
  std::shared_ptr<ModelStreamerRestore> AcquireRestore() const;

  Path storage_root_;
  PosixCopyEngine posix_;
  mutable std::mutex restore_mutex_;
  mutable std::shared_ptr<ModelStreamerRestore> restore_;
};
}  // namespace snapshot::pagebroker

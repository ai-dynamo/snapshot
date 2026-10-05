// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cstdint>
#include <filesystem>

#include "pagebroker_types.hpp"
#include "transfer/restore_plan.hpp"
#include "transfer/transfer_control.hpp"

namespace snapshot::pagebroker {
using Path = std::filesystem::path;

enum class TransferEngineType { POSIX_COPY, MODEL_STREAMER };

class TransferEngine {
 public:
  virtual ~TransferEngine();
  virtual TransferEngineType type() const = 0;
  virtual RestorePlan PrepareRestore(const StorageBackend& source, TransferControl control = {}) const = 0;
  virtual void StageRestore(const RestorePlan& plan, const Path& destination, TransferControl control = {}) const = 0;
  virtual void ValidateCheckpointDestination(const StorageBackend& destination) const = 0;
  virtual bool CheckpointDestinationConflicts(const StorageBackend& destination) const = 0;
  virtual void PublishCheckpoint(const Path& source, const StorageBackend& destination) const = 0;
};
}  // namespace snapshot::pagebroker

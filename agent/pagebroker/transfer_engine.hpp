// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cstdint>
#include <filesystem>
#include <stdexcept>

#include "pagebroker_types.hpp"

namespace snapshot::pagebroker {
using Path = std::filesystem::path;

enum class TransferEngineType { POSIX_COPY };

class StagingCapacityExceeded : public std::runtime_error {
 public:
  StagingCapacityExceeded() : std::runtime_error("restore source exceeded its admitted staging bytes") {}
};

class TransferEngine {
 public:
  virtual ~TransferEngine();
  virtual TransferEngineType type() const = 0;
  virtual uintmax_t RestoreSize(const StorageBackend& source) const = 0;
  // Returns the number of regular-file bytes actually copied. The source can
  // change after RestoreSize(), so callers must validate this value before
  // exposing the staged directory.
  virtual uintmax_t StageRestore(
      const StorageBackend& source, const Path& destination,
      uintmax_t max_bytes) const = 0;
  virtual void ValidateCheckpointDestination(const StorageBackend& destination) const = 0;
  virtual bool CheckpointDestinationConflicts(const StorageBackend& destination) const = 0;
  virtual void PublishCheckpoint(const Path& source, const StorageBackend& destination) const = 0;
  virtual uintmax_t CopyDirectory(const Path& source, const Path& destination) const = 0;
};
}  // namespace snapshot::pagebroker

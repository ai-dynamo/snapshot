// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <filesystem>

namespace snapshot::pagebroker {
using Path = std::filesystem::path;

enum class TransferEngineType { POSIX_COPY };

class TransferEngine {
 public:
  virtual ~TransferEngine();
  virtual TransferEngineType type() const = 0;
  virtual void CopyDirectory(const Path& source, const Path& destination) const = 0;
};
}  // namespace snapshot::pagebroker

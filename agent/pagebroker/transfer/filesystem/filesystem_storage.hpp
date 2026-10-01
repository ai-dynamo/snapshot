// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cstdint>
#include <filesystem>

#include "pagebroker_types.hpp"

namespace snapshot::pagebroker::filesystem_storage {
using Path = std::filesystem::path;

// Shared filesystem storage operations. Storage paths must be beneath the
// canonical storage root and contain no symlink components.
Path SourcePath(const StorageBackend& source, const Path& storage_root);
Path DestinationPath(const StorageBackend& destination, const Path& storage_root);
uintmax_t RestoreSize(const StorageBackend& source, const Path& storage_root);
bool CheckpointDestinationConflicts(const StorageBackend& destination, const Path& storage_root);
void PublishCheckpoint(const Path& source, const StorageBackend& destination, const Path& storage_root);
}  // namespace snapshot::pagebroker::filesystem_storage

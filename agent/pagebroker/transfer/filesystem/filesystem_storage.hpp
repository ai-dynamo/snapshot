// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cstdint>
#include <filesystem>
#include <limits>

#include "pagebroker_types.hpp"
#include "transfer/restore_plan.hpp"
#include "cancellation.hpp"

namespace snapshot::pagebroker::filesystem_storage {
using Path = std::filesystem::path;

// Returns an existing source directory beneath storage_root, rejecting symlink components.
Path SourcePath(const StorageBackend& source, const Path& storage_root);

// Returns a destination beneath storage_root, rejecting symlink components.
Path DestinationPath(const StorageBackend& destination, const Path& storage_root);

// Creates a new directory with permissions filtered by umask; parents must exist.
// Any existing final entry, including a symlink, is a transaction conflict.
void CreateDirectoryExclusive(const Path& directory, std::filesystem::perms permissions);

// Removes an owned staging tree, restoring directory access without following links.
void RemoveStaging(const Path& directory);

// Copies the planned files into staging and applies saved permissions.
void StageRestore(const RestorePlan& plan, const Path& destination, Cancellation control = {});

// Scans a checkpoint tree into a restore plan, enforcing the entry limit.
RestorePlan BuildRestorePlan(
    const Path& source, Cancellation control = {}, std::size_t limit = std::numeric_limits<std::size_t>::max());

// Builds a CPU restore plan that omits the payload directory owned by the GPU engine.
RestorePlan BuildCpuRestorePlan(const Path& source, Cancellation control = {});

// Opens a local directory without following its final symlink.
FileDescriptor OpenDirectory(const Path& directory);

// Retains the source descriptor and scans files only when staging is requested.
PreparedRestore PrepareRestore(const StorageBackend& source, const Path& storage_root,
    TransferMode mode, Cancellation control = {});

// Returns local access, borrowing the original GPU source when one was prepared.
RestoreAccess CompleteRestore(PreparedRestore preparation, const Path& destination);

// Selects staging or a transaction-specific directory beside the final destination.
Path PrepareCheckpoint(const StorageBackend& destination, const Path& storage_root,
    const Path& staging_directory, const std::string& transaction_id, TransferMode mode);

// Publishes by direct rename or staged copy, checking cancellation before promotion.
void PublishCheckpoint(const Path& source, const StorageBackend& destination, const Path& storage_root,
    const RestorePlan& plan, TransferMode mode, Cancellation control = {});
}  // namespace snapshot::pagebroker::filesystem_storage

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <string_view>

#include "pagebroker_types.hpp"
#include "transfer/engine/transfer_engine.hpp"
#include "transfer/checkpoint_inventory.hpp"

namespace snapshot::pagebroker {
// Version of the published checkpoint index.
inline constexpr char kCheckpointFormat[] = "snapshot.s3-checkpoint/v1";
// Metadata object name within a checkpoint storage prefix.
inline constexpr char kCheckpointIndexName[] = "index.json";
// Required metadata entry.
inline constexpr char kCheckpointManifestName[] = "manifest.yaml";
// Bounds the JSON metadata fetched from S3 before it can become a restore inventory.
inline constexpr std::size_t kMaxIndexBytes = 4 * 1024 * 1024;
// Limits the required manifest and the total entries/bytes described by an index.
inline constexpr std::size_t kMaxManifestBytes = 1024 * 1024;
inline constexpr std::size_t kMaxCheckpointEntries = 10000;
inline constexpr std::uint64_t kMaxCheckpointBytes = 1ULL << 40;

// Carries protocol failure codes for corrupt, unsupported or conflicting artifacts.
using CheckpointError = TransferError;

// Validates ordered, unique relative paths, permissions, manifest and size limits,
// then returns total file bytes.
std::uint64_t ValidateCheckpointInventory(const CheckpointInventory& inventory);
// Validates and encodes the inventory's tree metadata as bounded JSON.
// Source URLs are excluded so the configured store supplies locations on restore.
std::string SerializeCheckpointIndex(const CheckpointInventory& inventory);
// Decodes and validates an index, leaving source URLs for the backend to populate.
// Reports unsupported versions separately from corrupt content via CheckpointError.
CheckpointInventory ParseCheckpointIndex(std::string_view text);
}  // namespace snapshot::pagebroker

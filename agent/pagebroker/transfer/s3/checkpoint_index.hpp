// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <string_view>

#include "pagebroker_types.hpp"
#include "transfer/engine/transfer_engine.hpp"
#include "transfer/restore_plan.hpp"

namespace snapshot::pagebroker {
// Version written into both the published descriptor and the checkpoint index.
inline constexpr char kCheckpointFormat[] = "snapshot.s3-checkpoint/v1";
// Bounds the JSON metadata fetched from S3 before it can become a restore plan.
inline constexpr std::size_t kMaxIndexBytes = 4 * 1024 * 1024;
// Limits the required manifest and the total entries/bytes described by an index.
inline constexpr std::size_t kMaxManifestBytes = 1024 * 1024;
inline constexpr std::size_t kMaxCheckpointEntries = 10000;
inline constexpr std::uint64_t kMaxCheckpointBytes = 1ULL << 40;

// Carries protocol failure codes for corrupt, unsupported or conflicting artifacts.
using CheckpointError = TransferError;

// Requires the fixed lowercase hexadecimal handle used in artifact object keys.
void ValidateCheckpointID(std::string_view id);
// Validates ordered, unique relative paths, permissions, manifest and size limits,
// then returns total file bytes. Preflight callers may omit not-yet-known digests.
std::uint64_t ValidateCheckpointPlan(const RestorePlan& plan, bool require_digests = true);
// Validates and encodes the plan's tree metadata and digests as bounded JSON.
// Source URLs are excluded so the configured store supplies locations on restore.
std::string SerializeCheckpointIndex(const RestorePlan& plan);
// Decodes and validates an index, leaving source URLs for the backend to populate.
// Reports unsupported versions separately from corrupt content via CheckpointError.
RestorePlan ParseCheckpointIndex(std::string_view text);
}  // namespace snapshot::pagebroker

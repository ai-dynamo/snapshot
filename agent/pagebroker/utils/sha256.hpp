// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <filesystem>
#include <span>

namespace snapshot::pagebroker::utils {
using Sha256Digest = std::array<std::uint8_t, 32>;

Sha256Digest ComputeSha256(std::span<const std::byte> bytes);
// Reads a regular file with bounded buffering. The caller must keep it unchanged
// until the operation completes. Symlinks and special files are rejected.
Sha256Digest ComputeFileSha256(const std::filesystem::path& path);
}  // namespace snapshot::pagebroker::utils

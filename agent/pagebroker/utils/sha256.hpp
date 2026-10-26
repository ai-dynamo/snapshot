// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <span>

namespace snapshot::pagebroker::utils {
// Raw SHA-256 digest for deterministic artifact identity derivation.
using Sha256Digest = std::array<std::uint8_t, 32>;

// Hashes an in-memory identity encoding; checkpoint payloads are not hashed.
Sha256Digest ComputeSha256(std::span<const std::byte> bytes);
}  // namespace snapshot::pagebroker::utils

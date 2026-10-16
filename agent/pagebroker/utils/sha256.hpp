// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <filesystem>
#include <functional>
#include <span>

namespace snapshot::pagebroker::utils {
// Raw SHA-256 digest for payload verification.
using Sha256Digest = std::array<std::uint8_t, 32>;

// Hashes borrowed memory; calls check between chunks and before finalization.
Sha256Digest ComputeSha256(std::span<const std::byte> bytes, const std::function<void()>& check = {});

// Hashes an unchanged regular file with bounded buffering, rejecting symlinks.
Sha256Digest ComputeFileSha256(const std::filesystem::path& path);

// Hashes an unchanged borrowed file without moving its offset; check follows ComputeSha256.
Sha256Digest ComputeFileSha256(int descriptor, const std::function<void()>& check = {});
}  // namespace snapshot::pagebroker::utils

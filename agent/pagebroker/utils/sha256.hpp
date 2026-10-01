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
// Raw SHA-256 bytes used to compare restored payloads with their expected digest.
using Sha256Digest = std::array<std::uint8_t, 32>;

// Hashes borrowed memory in chunks. The optional callback runs between chunks
// and before finalization so callers can enforce cancellation or a deadline;
// exceptions from the callback propagate to the caller.
Sha256Digest ComputeSha256(std::span<const std::byte> bytes, const std::function<void()>& check = {});
// Reads a regular file with bounded buffering. The caller must keep it unchanged
// until the operation completes. Symlinks and special files are rejected.
Sha256Digest ComputeFileSha256(const std::filesystem::path& path);
// Borrows an open regular file without closing it or changing its seek offset.
// The file must remain unchanged; the optional callback has the same stop-check
// contract as ComputeSha256 and runs between reads and before finalization.
Sha256Digest ComputeFileSha256(int descriptor, const std::function<void()>& check = {});
}  // namespace snapshot::pagebroker::utils

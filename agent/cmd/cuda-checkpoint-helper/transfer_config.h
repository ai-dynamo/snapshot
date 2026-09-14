/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#pragma once

#include <cstddef>
#include <cstdint>
#include <filesystem>
#include <string>
#include <string_view>
#include <vector>

namespace cuda_checkpoint_transfer {

enum class TransferOperation {
  kCheckpoint,
  kRestore,
};

constexpr size_t kDefaultBufferCount = 1;
constexpr size_t kDefaultChunkBytes = 64ULL * 1024ULL * 1024ULL;
constexpr size_t kMinimumChunkBytes = 1ULL * 1024ULL * 1024ULL;
constexpr size_t kMaximumChunkBytes = 256ULL * 1024ULL * 1024ULL;
constexpr size_t kMaximumBufferCount = 8;
constexpr size_t kMaximumPinnedBytesPerDevice =
    1ULL * 1024ULL * 1024ULL * 1024ULL;
// Peak process budget enforced by TransferBatch's bounded execution waves.
// Aggregate accepted work may exceed this value.
constexpr size_t kMaximumPinnedBytesPerOperation =
    2ULL * 1024ULL * 1024ULL * 1024ULL;
constexpr size_t kMaximumTransferChunkCount = 1024ULL * 1024ULL;
constexpr size_t kBufferAlignment = 4096;

struct TransferOptions {
  size_t buffer_count = kDefaultBufferCount;
  size_t chunk_bytes = kDefaultChunkBytes;
};

struct StorageFile {
  std::filesystem::path path;
  size_t size = 0;
  // When nonnegative, path is a single relative leaf opened beneath this
  // already-validated directory descriptor. The caller retains ownership for
  // the complete transfer.
  int directory_fd = -1;
  // When nonnegative, this is the exact already-open source file. The caller
  // retains ownership; transfer duplicates and verifies its bound identity.
  int descriptor_fd = -1;
  uint64_t device = 0;
  uint64_t inode = 0;
};

struct StorageRange {
  size_t logical_offset = 0;
  size_t size = 0;
  size_t file_index = 0;
  size_t file_offset = 0;
};

struct StorageLayout {
  std::vector<StorageFile> files;
  std::vector<StorageRange> ranges;
};

struct TransferChunk {
  size_t logical_offset;
  size_t size;
  size_t file_index;
  size_t file_offset;
  size_t slot_index;
};

bool ParseSize(std::string_view value, size_t *parsed);
bool ValidateTransferOptions(const TransferOptions &options,
                             std::string *error);
bool CalculatePinnedBytes(size_t device_count, const TransferOptions &options,
                          size_t *bytes, std::string *error);
bool CalculateBatchPinnedBytes(const std::vector<size_t> &target_bytes,
                               size_t *bytes, std::string *error);
int StorageFileOpenFlags(TransferOperation operation);
bool BuildTransferChunks(size_t extent_size, const StorageLayout &storage,
                         const TransferOptions &options,
                         std::vector<TransferChunk> *chunks,
                         std::string *error);
bool BuildContiguousStorageLayout(const std::filesystem::path &base_path,
                                  size_t extent_size, size_t file_count,
                                  StorageLayout *storage, std::string *error);
std::string JsonEscape(std::string_view value);

} // namespace cuda_checkpoint_transfer

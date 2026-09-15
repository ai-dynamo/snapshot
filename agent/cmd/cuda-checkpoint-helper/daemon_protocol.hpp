/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#pragma once

#include <cstddef>
#include <cstdint>
#include <span>
#include <string>
#include <vector>

namespace cuda_checkpoint_daemon {

// This protocol is private to the co-versioned PageBroker and CUDA helper.
// It carries CUDA operation data, not Snapshot or PageBroker transaction IDs.
constexpr uint32_t kProtocolMagic = 0x50484344; // "DCHP" in little-endian.
constexpr uint16_t kProtocolVersion = 1;
// Keep the proven 76-byte envelope stable. Bytes 56-59 are the batch payload
// length and remain zero until a later restore-batch protocol revision uses
// them; decoders reject nonzero values in this version.
constexpr size_t kRequestHeaderSize = 76;
constexpr size_t kResponseHeaderSize = 24;
constexpr size_t kMaxRequestSize = 64 * 1024;
constexpr size_t kMaxResponseSize = 128 * 1024;
constexpr size_t kMaxCgroupSize = 4096;
constexpr size_t kMaxJobFileSize = 4096;

constexpr uint32_t kResponseFatal = 1U << 0;
constexpr uint32_t kResponseCapabilityDeferredCUDA = 1U << 1;
constexpr uint32_t kResponseCapabilityCustomStorage = 1U << 2;
constexpr uint32_t kResponseLockNotAcquired = 1U << 3;

enum class Action : uint16_t {
  kHealth = 0,
  kCheckpoint = 1,
  kRestore = 2,
  kLock = 3,
  kUnlock = 4,
};

enum class Backend : uint16_t {
  kUnspecified = 0,
  kRegular = 1,
  kPosix = 2,
};

struct Request {
  Action action = Action::kHealth;
  Backend backend = Backend::kUnspecified;
  uint32_t pid = 0;
  uint32_t transfer_buffer_count = 0;
  uint64_t transfer_chunk_bytes = 0;
  uint64_t expected_start_time_ticks = 0;
  std::string device_map;
  std::string storage_dir;
  std::string expected_cgroup;
  std::string job_file;
  // Optional identity observed for job_file. {0, 0} means unpinned; a nonzero
  // device requires a nonzero inode. The codec does not inspect the path.
  // Consumers that require a pinned launch-job file must reject {0, 0} and
  // revalidate this pair at their final process-dispatch boundary.
  uint64_t expected_job_file_device = 0;
  uint64_t expected_job_file_inode = 0;
  std::string selected_devices;
};

struct Response {
  int32_t cuda_status = 0;
  uint32_t flags = 0;
  std::string output;
  std::string error;
};

// These functions validate only the bounded wire representation. Command
// semantics, process identity, path admission, and execution policy belong to
// the daemon server and PageBroker adapters that consume this codec.
bool EncodeRequest(const Request &request, std::vector<uint8_t> *encoded,
                   std::string *error);
bool DecodeRequest(std::span<const uint8_t> encoded, Request *request,
                   std::string *error);
bool EncodeResponse(const Response &response, std::vector<uint8_t> *encoded,
                    std::string *error);
bool DecodeResponse(std::span<const uint8_t> encoded, Response *response,
                    std::string *error);

} // namespace cuda_checkpoint_daemon

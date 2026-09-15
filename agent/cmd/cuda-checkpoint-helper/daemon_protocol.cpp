/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#include "daemon_protocol.hpp"

#include <bit>
#include <cstring>
#include <limits>
#include <string_view>

namespace cuda_checkpoint_daemon {
namespace {

constexpr size_t kMagicOffset = 0;
constexpr size_t kVersionOffset = 4;
constexpr size_t kHeaderSizeOffset = 6;
constexpr size_t kActionOffset = 8;
constexpr size_t kBackendOffset = 10;
constexpr size_t kPidOffset = 12;
constexpr size_t kTransferBufferCountOffset = 16;
constexpr size_t kTransferChunkBytesOffset = 20;
constexpr size_t kDeviceMapSizeOffset = 28;
constexpr size_t kStorageDirectorySizeOffset = 32;
constexpr size_t kCgroupSizeOffset = 36;
constexpr size_t kStartTimeOffset = 40;
constexpr size_t kJobFileSizeOffset = 48;
constexpr size_t kSelectedDevicesSizeOffset = 52;
constexpr size_t kBatchPayloadSizeOffset = 56;
constexpr size_t kJobFileDeviceOffset = 60;
constexpr size_t kJobFileInodeOffset = 68;

constexpr size_t kResponseStatusOffset = 8;
constexpr size_t kResponseFlagsOffset = 12;
constexpr size_t kResponseOutputSizeOffset = 16;
constexpr size_t kResponseErrorSizeOffset = 20;

constexpr uint32_t kKnownResponseFlags =
    kResponseFatal | kResponseCapabilityDeferredCUDA |
    kResponseCapabilityCustomStorage | kResponseLockNotAcquired;

static_assert(kJobFileInodeOffset + sizeof(uint64_t) == kRequestHeaderSize);
static_assert(kResponseErrorSizeOffset + sizeof(uint32_t) ==
              kResponseHeaderSize);

void SetError(std::string *error, std::string_view message) {
  if (error != nullptr) {
    error->assign(message);
  }
}

uint16_t ReadU16(std::span<const uint8_t> data, size_t offset) {
  return static_cast<uint16_t>(data[offset]) |
         static_cast<uint16_t>(data[offset + 1]) << 8;
}

uint32_t ReadU32(std::span<const uint8_t> data, size_t offset) {
  return static_cast<uint32_t>(data[offset]) |
         static_cast<uint32_t>(data[offset + 1]) << 8 |
         static_cast<uint32_t>(data[offset + 2]) << 16 |
         static_cast<uint32_t>(data[offset + 3]) << 24;
}

uint64_t ReadU64(std::span<const uint8_t> data, size_t offset) {
  return static_cast<uint64_t>(ReadU32(data, offset)) |
         static_cast<uint64_t>(ReadU32(data, offset + 4)) << 32;
}

void WriteU16(std::vector<uint8_t> *data, size_t offset, uint16_t value) {
  (*data)[offset] = static_cast<uint8_t>(value);
  (*data)[offset + 1] = static_cast<uint8_t>(value >> 8);
}

void WriteU32(std::vector<uint8_t> *data, size_t offset, uint32_t value) {
  for (size_t index = 0; index < sizeof(value); ++index) {
    (*data)[offset + index] =
        static_cast<uint8_t>(value >> static_cast<unsigned>(index * 8));
  }
}

void WriteU64(std::vector<uint8_t> *data, size_t offset, uint64_t value) {
  WriteU32(data, offset, static_cast<uint32_t>(value));
  WriteU32(data, offset + 4, static_cast<uint32_t>(value >> 32));
}

bool IsKnown(Action action) {
  switch (action) {
  case Action::kHealth:
  case Action::kCheckpoint:
  case Action::kRestore:
  case Action::kLock:
  case Action::kUnlock:
    return true;
  }
  return false;
}

bool IsKnown(Backend backend) {
  switch (backend) {
  case Backend::kUnspecified:
  case Backend::kRegular:
  case Backend::kPosix:
    return true;
  }
  return false;
}

bool ContainsNul(std::string_view value) {
  return value.find('\0') != std::string_view::npos;
}

bool ValidateRequestStrings(const Request &request, size_t *encoded_size,
                            std::string *error) {
  const std::string_view fields[] = {
      request.device_map, request.storage_dir, request.expected_cgroup,
      request.job_file, request.selected_devices,
  };
  for (const std::string_view field : fields) {
    if (ContainsNul(field)) {
      SetError(error, "request strings contain NUL");
      return false;
    }
  }
  if (request.expected_cgroup.size() > kMaxCgroupSize) {
    SetError(error, "request cgroup is too large");
    return false;
  }
  if (request.job_file.size() > kMaxJobFileSize) {
    SetError(error, "request job file is too large");
    return false;
  }
  if ((request.job_file.empty() &&
       (request.expected_job_file_device != 0 ||
        request.expected_job_file_inode != 0)) ||
      (request.expected_job_file_device != 0 &&
       request.expected_job_file_inode == 0)) {
    SetError(error, "request has an invalid job file identity");
    return false;
  }
  size_t size = kRequestHeaderSize;
  for (const std::string_view field : fields) {
    if (field.size() > std::numeric_limits<uint32_t>::max() ||
        field.size() > kMaxRequestSize - size) {
      SetError(error, "request is too large");
      return false;
    }
    size += field.size();
  }
  *encoded_size = size;
  return true;
}

bool HasRequestHeader(std::span<const uint8_t> encoded, std::string *error) {
  if (encoded.size() < kRequestHeaderSize ||
      encoded.size() > kMaxRequestSize) {
    SetError(error, "invalid request size");
    return false;
  }
  if (ReadU32(encoded, kMagicOffset) != kProtocolMagic ||
      ReadU16(encoded, kVersionOffset) != kProtocolVersion ||
      ReadU16(encoded, kHeaderSizeOffset) != kRequestHeaderSize) {
    SetError(error, "invalid request protocol header");
    return false;
  }
  return true;
}

bool HasResponseHeader(std::span<const uint8_t> encoded, std::string *error) {
  if (encoded.size() < kResponseHeaderSize ||
      encoded.size() > kMaxResponseSize) {
    SetError(error, "invalid response size");
    return false;
  }
  if (ReadU32(encoded, kMagicOffset) != kProtocolMagic ||
      ReadU16(encoded, kVersionOffset) != kProtocolVersion ||
      ReadU16(encoded, kHeaderSizeOffset) != kResponseHeaderSize) {
    SetError(error, "invalid response protocol header");
    return false;
  }
  return true;
}

void Append(std::vector<uint8_t> *encoded, size_t *offset,
            std::string_view value) {
  if (value.empty()) {
    return;
  }
  std::memcpy(encoded->data() + *offset, value.data(), value.size());
  *offset += value.size();
}

std::string ReadString(std::span<const uint8_t> encoded, size_t *offset,
                       size_t size) {
  const char *data = reinterpret_cast<const char *>(encoded.data() + *offset);
  *offset += size;
  return std::string(data, size);
}

} // namespace

bool EncodeRequest(const Request &request, std::vector<uint8_t> *encoded,
                   std::string *error) {
  if (encoded == nullptr) {
    SetError(error, "request output is required");
    return false;
  }
  if (!IsKnown(request.action) || !IsKnown(request.backend)) {
    SetError(error, "request has an unknown action or backend");
    return false;
  }
  size_t encoded_size = 0;
  if (!ValidateRequestStrings(request, &encoded_size, error)) {
    return false;
  }

  std::vector<uint8_t> result(encoded_size, 0);
  WriteU32(&result, kMagicOffset, kProtocolMagic);
  WriteU16(&result, kVersionOffset, kProtocolVersion);
  WriteU16(&result, kHeaderSizeOffset,
           static_cast<uint16_t>(kRequestHeaderSize));
  WriteU16(&result, kActionOffset, static_cast<uint16_t>(request.action));
  WriteU16(&result, kBackendOffset, static_cast<uint16_t>(request.backend));
  WriteU32(&result, kPidOffset, request.pid);
  WriteU32(&result, kTransferBufferCountOffset,
           request.transfer_buffer_count);
  WriteU64(&result, kTransferChunkBytesOffset, request.transfer_chunk_bytes);
  WriteU32(&result, kDeviceMapSizeOffset,
           static_cast<uint32_t>(request.device_map.size()));
  WriteU32(&result, kStorageDirectorySizeOffset,
           static_cast<uint32_t>(request.storage_dir.size()));
  WriteU32(&result, kCgroupSizeOffset,
           static_cast<uint32_t>(request.expected_cgroup.size()));
  WriteU64(&result, kStartTimeOffset, request.expected_start_time_ticks);
  WriteU32(&result, kJobFileSizeOffset,
           static_cast<uint32_t>(request.job_file.size()));
  WriteU32(&result, kSelectedDevicesSizeOffset,
           static_cast<uint32_t>(request.selected_devices.size()));
  WriteU32(&result, kBatchPayloadSizeOffset, 0);
  WriteU64(&result, kJobFileDeviceOffset,
           request.expected_job_file_device);
  WriteU64(&result, kJobFileInodeOffset, request.expected_job_file_inode);
  size_t offset = kRequestHeaderSize;
  Append(&result, &offset, request.device_map);
  Append(&result, &offset, request.storage_dir);
  Append(&result, &offset, request.expected_cgroup);
  Append(&result, &offset, request.job_file);
  Append(&result, &offset, request.selected_devices);
  *encoded = std::move(result);
  return true;
}

bool DecodeRequest(std::span<const uint8_t> encoded, Request *request,
                   std::string *error) {
  if (request == nullptr) {
    SetError(error, "request output is required");
    return false;
  }
  if (!HasRequestHeader(encoded, error)) {
    return false;
  }
  const auto action = static_cast<Action>(ReadU16(encoded, kActionOffset));
  const auto backend = static_cast<Backend>(ReadU16(encoded, kBackendOffset));
  if (!IsKnown(action) || !IsKnown(backend)) {
    SetError(error, "request has an unknown action or backend");
    return false;
  }

  const uint32_t device_map_size = ReadU32(encoded, kDeviceMapSizeOffset);
  const uint32_t storage_dir_size =
      ReadU32(encoded, kStorageDirectorySizeOffset);
  const uint32_t cgroup_size = ReadU32(encoded, kCgroupSizeOffset);
  const uint32_t job_file_size = ReadU32(encoded, kJobFileSizeOffset);
  const uint32_t selected_devices_size =
      ReadU32(encoded, kSelectedDevicesSizeOffset);
  const uint64_t payload_size =
      static_cast<uint64_t>(device_map_size) + storage_dir_size + cgroup_size +
      job_file_size + selected_devices_size;
  if (ReadU32(encoded, kBatchPayloadSizeOffset) != 0) {
    SetError(error, "request has a batch payload in a single-operation frame");
    return false;
  }
  if (cgroup_size > kMaxCgroupSize || job_file_size > kMaxJobFileSize ||
      payload_size != encoded.size() - kRequestHeaderSize) {
    SetError(error, "invalid request payload lengths");
    return false;
  }

  Request result;
  result.action = action;
  result.backend = backend;
  result.pid = ReadU32(encoded, kPidOffset);
  result.transfer_buffer_count = ReadU32(encoded, kTransferBufferCountOffset);
  result.transfer_chunk_bytes = ReadU64(encoded, kTransferChunkBytesOffset);
  result.expected_start_time_ticks = ReadU64(encoded, kStartTimeOffset);
  result.expected_job_file_device = ReadU64(encoded, kJobFileDeviceOffset);
  result.expected_job_file_inode = ReadU64(encoded, kJobFileInodeOffset);
  size_t offset = kRequestHeaderSize;
  result.device_map = ReadString(encoded, &offset, device_map_size);
  result.storage_dir = ReadString(encoded, &offset, storage_dir_size);
  result.expected_cgroup = ReadString(encoded, &offset, cgroup_size);
  result.job_file = ReadString(encoded, &offset, job_file_size);
  result.selected_devices =
      ReadString(encoded, &offset, selected_devices_size);
  size_t ignored_size = 0;
  if (!ValidateRequestStrings(result, &ignored_size, error)) {
    return false;
  }
  *request = std::move(result);
  return true;
}

bool EncodeResponse(const Response &response, std::vector<uint8_t> *encoded,
                    std::string *error) {
  if (encoded == nullptr) {
    SetError(error, "response output is required");
    return false;
  }
  if ((response.flags & ~kKnownResponseFlags) != 0) {
    SetError(error, "response has unknown flags");
    return false;
  }
  if (response.output.size() > std::numeric_limits<uint32_t>::max() ||
      response.error.size() > std::numeric_limits<uint32_t>::max() ||
      response.output.size() > kMaxResponseSize - kResponseHeaderSize ||
      response.error.size() >
          kMaxResponseSize - kResponseHeaderSize - response.output.size()) {
    SetError(error, "response is too large");
    return false;
  }

  std::vector<uint8_t> result(kResponseHeaderSize + response.output.size() +
                                  response.error.size(),
                              0);
  WriteU32(&result, kMagicOffset, kProtocolMagic);
  WriteU16(&result, kVersionOffset, kProtocolVersion);
  WriteU16(&result, kHeaderSizeOffset,
           static_cast<uint16_t>(kResponseHeaderSize));
  WriteU32(&result, kResponseStatusOffset,
           std::bit_cast<uint32_t>(response.cuda_status));
  WriteU32(&result, kResponseFlagsOffset, response.flags);
  WriteU32(&result, kResponseOutputSizeOffset,
           static_cast<uint32_t>(response.output.size()));
  WriteU32(&result, kResponseErrorSizeOffset,
           static_cast<uint32_t>(response.error.size()));
  size_t offset = kResponseHeaderSize;
  Append(&result, &offset, response.output);
  Append(&result, &offset, response.error);
  *encoded = std::move(result);
  return true;
}

bool DecodeResponse(std::span<const uint8_t> encoded, Response *response,
                    std::string *error) {
  if (response == nullptr) {
    SetError(error, "response output is required");
    return false;
  }
  if (!HasResponseHeader(encoded, error)) {
    return false;
  }
  const uint32_t flags = ReadU32(encoded, kResponseFlagsOffset);
  if ((flags & ~kKnownResponseFlags) != 0) {
    SetError(error, "response has unknown flags");
    return false;
  }
  const uint32_t output_size = ReadU32(encoded, kResponseOutputSizeOffset);
  const uint32_t error_size = ReadU32(encoded, kResponseErrorSizeOffset);
  if (static_cast<uint64_t>(output_size) + error_size !=
      encoded.size() - kResponseHeaderSize) {
    SetError(error, "invalid response payload lengths");
    return false;
  }

  Response result;
  result.cuda_status =
      std::bit_cast<int32_t>(ReadU32(encoded, kResponseStatusOffset));
  result.flags = flags;
  size_t offset = kResponseHeaderSize;
  result.output = ReadString(encoded, &offset, output_size);
  result.error = ReadString(encoded, &offset, error_size);
  *response = std::move(result);
  return true;
}

} // namespace cuda_checkpoint_daemon

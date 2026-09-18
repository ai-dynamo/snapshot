/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#include "daemon_protocol.hpp"

#include <iostream>
#include <limits>
#include <span>
#include <string>
#include <string_view>
#include <vector>

namespace protocol = cuda_checkpoint_daemon;

namespace {

bool Check(bool condition, std::string_view message) {
  if (!condition) {
    std::cerr << message << '\n';
  }
  return condition;
}

protocol::Request ExampleRequest() {
  return {
      .action = protocol::Action::kRestore,
      .backend = protocol::Backend::kPosix,
      .pid = 0x01020304,
      .transfer_buffer_count = 2,
      .transfer_chunk_bytes = 0x0102030405060708ULL,
      .expected_start_time_ticks = 0x1112131415161718ULL,
      .device_map = "d",
      .storage_dir = "st",
      .expected_cgroup = "cg\n",
      .job_file = "/j",
      .expected_job_file_device = 0x2122232425262728ULL,
      .expected_job_file_inode = 0x3132333435363738ULL,
      .selected_devices = "g",
  };
}

std::string Hex(std::span<const uint8_t> bytes) {
  constexpr char kDigits[] = "0123456789abcdef";
  std::string encoded;
  encoded.reserve(bytes.size() * 2);
  for (const uint8_t byte : bytes) {
    encoded.push_back(kDigits[byte >> 4]);
    encoded.push_back(kDigits[byte & 0xf]);
  }
  return encoded;
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

bool SameRequest(const protocol::Request &left,
                 const protocol::Request &right) {
  return left.action == right.action && left.backend == right.backend &&
         left.pid == right.pid &&
         left.transfer_buffer_count == right.transfer_buffer_count &&
         left.transfer_chunk_bytes == right.transfer_chunk_bytes &&
         left.expected_start_time_ticks == right.expected_start_time_ticks &&
         left.device_map == right.device_map &&
         left.storage_dir == right.storage_dir &&
         left.expected_cgroup == right.expected_cgroup &&
         left.job_file == right.job_file &&
         left.selected_devices == right.selected_devices &&
         left.expected_job_file_device == right.expected_job_file_device &&
         left.expected_job_file_inode == right.expected_job_file_inode;
}

bool TestRequestGoldenAndRoundTrip() {
  const protocol::Request request = ExampleRequest();
  std::vector<uint8_t> encoded;
  std::string error;
  if (!Check(protocol::EncodeRequest(request, &encoded, &error), error)) {
    return false;
  }
  constexpr std::string_view kGolden =
      "44434850"             // magic
      "0100"                 // version
      "4c00"                 // 76-byte header
      "0200"                 // restore
      "0200"                 // POSIX
      "04030201"             // PID
      "02000000"             // transfer buffers
      "0807060504030201"     // transfer chunk bytes
      "01000000"             // device map bytes
      "02000000"             // storage directory bytes
      "03000000"             // cgroup bytes
      "1817161514131211"     // process start time
      "02000000"             // job file bytes
      "01000000"             // selected device bytes
      "00000000"             // reserved batch payload bytes
      "2827262524232221"     // job file device
      "3837363534333231"     // job file inode
      "64737463670a2f6a67";  // payload
  if (!Check(Hex(encoded) == kGolden, "request wire bytes changed")) {
    return false;
  }

  protocol::Request decoded;
  return Check(protocol::DecodeRequest(encoded, &decoded, &error), error) &&
         Check(SameRequest(decoded, request), "request round trip changed data");
}

bool TestRequestBoundsAndMalformedFrames() {
  protocol::Request request = ExampleRequest();
  std::vector<uint8_t> encoded;
  std::string error;
  if (!protocol::EncodeRequest(request, &encoded, &error)) {
    return Check(false, error);
  }
  protocol::Request decoded;

  if (!Check(!protocol::DecodeRequest(
                 std::span(encoded).first(protocol::kRequestHeaderSize - 1),
                 &decoded, &error),
             "truncated request header was accepted") ||
      !Check(!protocol::DecodeRequest(
                 std::vector<uint8_t>(protocol::kMaxRequestSize + 1), &decoded,
                 &error),
             "oversized request was accepted")) {
    return false;
  }

  auto malformed = encoded;
  malformed[0] ^= 1;
  if (!Check(!protocol::DecodeRequest(malformed, &decoded, &error),
             "invalid request magic was accepted")) {
    return false;
  }
  malformed = encoded;
  WriteU16(&malformed, 4, protocol::kProtocolVersion + 1);
  if (!Check(!protocol::DecodeRequest(malformed, &decoded, &error),
             "unknown request version was accepted")) {
    return false;
  }
  malformed = encoded;
  WriteU16(&malformed, 6, protocol::kRequestHeaderSize - 1);
  if (!Check(!protocol::DecodeRequest(malformed, &decoded, &error),
             "invalid request header size was accepted")) {
    return false;
  }
  malformed = encoded;
  WriteU16(&malformed, 8, 99);
  if (!Check(!protocol::DecodeRequest(malformed, &decoded, &error),
             "unknown request action was accepted")) {
    return false;
  }
  malformed = encoded;
  WriteU16(&malformed, 10, 99);
  if (!Check(!protocol::DecodeRequest(malformed, &decoded, &error),
             "unknown request backend was accepted")) {
    return false;
  }
  malformed = encoded;
  WriteU32(&malformed, 28, 2);
  if (!Check(!protocol::DecodeRequest(malformed, &decoded, &error),
             "inconsistent request payload lengths were accepted")) {
    return false;
  }
  malformed = encoded;
  WriteU32(&malformed, 56, 1);
  if (!Check(!protocol::DecodeRequest(malformed, &decoded, &error),
             "nonzero reserved batch payload length was accepted")) {
    return false;
  }
  malformed = encoded;
  malformed[protocol::kRequestHeaderSize] = 0;
  if (!Check(!protocol::DecodeRequest(malformed, &decoded, &error),
             "request payload NUL was accepted")) {
    return false;
  }
  malformed = encoded;
  WriteU64(&malformed, 68, 0);
  if (!Check(!protocol::DecodeRequest(malformed, &decoded, &error),
             "job file device without an inode was decoded")) {
    return false;
  }

  protocol::Request empty_request;
  std::vector<uint8_t> oversized_field;
  if (!Check(protocol::EncodeRequest(empty_request, &oversized_field, &error),
             error)) {
    return false;
  }
  oversized_field.resize(protocol::kRequestHeaderSize +
                             protocol::kMaxCgroupSize + 1,
                         'c');
  WriteU32(&oversized_field, 36, protocol::kMaxCgroupSize + 1);
  if (!Check(!protocol::DecodeRequest(oversized_field, &decoded, &error),
             "oversized cgroup with a valid total length was decoded")) {
    return false;
  }
  if (!Check(protocol::EncodeRequest(empty_request, &oversized_field, &error),
             error)) {
    return false;
  }
  oversized_field.resize(protocol::kRequestHeaderSize +
                             protocol::kMaxJobFileSize + 1,
                         'j');
  WriteU32(&oversized_field, 48, protocol::kMaxJobFileSize + 1);
  if (!Check(!protocol::DecodeRequest(oversized_field, &decoded, &error),
             "oversized job file with a valid total length was decoded")) {
    return false;
  }

  request.action = static_cast<protocol::Action>(99);
  if (!Check(!protocol::EncodeRequest(request, &encoded, &error),
             "unknown request action was encoded")) {
    return false;
  }
  request = ExampleRequest();
  request.expected_cgroup.assign(protocol::kMaxCgroupSize + 1, 'c');
  if (!Check(!protocol::EncodeRequest(request, &encoded, &error),
             "oversized cgroup was encoded")) {
    return false;
  }
  request = ExampleRequest();
  request.job_file.assign(protocol::kMaxJobFileSize + 1, 'j');
  if (!Check(!protocol::EncodeRequest(request, &encoded, &error),
             "oversized job file was encoded")) {
    return false;
  }
  request = ExampleRequest();
  request.job_file.clear();
  if (!Check(!protocol::EncodeRequest(request, &encoded, &error),
             "job file identity without a path was encoded")) {
    return false;
  }
  request = ExampleRequest();
  request.expected_job_file_inode = 0;
  if (!Check(!protocol::EncodeRequest(request, &encoded, &error),
             "job file device without an inode was encoded")) {
    return false;
  }
  request = ExampleRequest();
  request.device_map.assign(protocol::kMaxRequestSize, 'd');
  return Check(!protocol::EncodeRequest(request, &encoded, &error),
               "oversized request payload was encoded");
}

bool TestCodecLeavesCommandPolicyToConsumers() {
  protocol::Request request = ExampleRequest();
  request.action = protocol::Action::kHealth;
  request.backend = protocol::Backend::kRegular;
  std::vector<uint8_t> encoded;
  std::string error;
  protocol::Request decoded;
  return Check(protocol::EncodeRequest(request, &encoded, &error), error) &&
         Check(protocol::DecodeRequest(encoded, &decoded, &error), error) &&
         Check(SameRequest(request, decoded),
               "framing layer changed opaque command fields");
}

bool TestEmptyPayloadRoundTrip() {
  const protocol::Request request;
  std::vector<uint8_t> encoded;
  std::string error;
  protocol::Request decoded;
  return Check(protocol::EncodeRequest(request, &encoded, &error), error) &&
         Check(encoded.size() == protocol::kRequestHeaderSize,
               "empty payload changed the request envelope size") &&
         Check(protocol::DecodeRequest(encoded, &decoded, &error), error) &&
         Check(SameRequest(request, decoded),
               "empty request round trip changed data");
}

bool TestResponseGoldenAndRoundTrip() {
  const protocol::Response response{
      .cuda_status = -7,
      .flags = protocol::kResponseFatal |
               protocol::kResponseCapabilityCustomStorage,
      .output = "ok",
      .error = "bad",
  };
  std::vector<uint8_t> encoded;
  std::string error;
  if (!Check(protocol::EncodeResponse(response, &encoded, &error), error)) {
    return false;
  }
  constexpr std::string_view kGolden =
      "44434850"          // magic
      "0100"              // version
      "1800"              // 24-byte header
      "f9ffffff"          // CUDA status -7
      "05000000"          // fatal + CustomStorage capability
      "02000000"          // output bytes
      "03000000"          // error bytes
      "6f6b626164";       // payload
  if (!Check(Hex(encoded) == kGolden, "response wire bytes changed")) {
    return false;
  }
  protocol::Response decoded;
  return Check(protocol::DecodeResponse(encoded, &decoded, &error), error) &&
         Check(decoded.cuda_status == response.cuda_status &&
                   decoded.flags == response.flags &&
                   decoded.output == response.output &&
                   decoded.error == response.error,
               "response round trip changed data");
}

bool TestResponseBoundsAndMalformedFrames() {
  protocol::Response response{
      .cuda_status = 0,
      .flags = protocol::kResponseCapabilityDeferredCUDA,
      .output = "output",
      .error = "error",
  };
  std::vector<uint8_t> encoded;
  std::string error;
  if (!protocol::EncodeResponse(response, &encoded, &error)) {
    return Check(false, error);
  }
  protocol::Response decoded;
  auto malformed = encoded;
  WriteU32(&malformed, 12, 1U << 31);
  if (!Check(!protocol::DecodeResponse(malformed, &decoded, &error),
             "unknown response flag was accepted")) {
    return false;
  }
  malformed = encoded;
  WriteU32(&malformed, 16, 1);
  if (!Check(!protocol::DecodeResponse(malformed, &decoded, &error),
             "inconsistent response lengths were accepted")) {
    return false;
  }
  if (!Check(!protocol::DecodeResponse(
                 std::span(encoded).first(protocol::kResponseHeaderSize - 1),
                 &decoded, &error),
             "truncated response header was accepted") ||
      !Check(!protocol::DecodeResponse(
                 std::vector<uint8_t>(protocol::kMaxResponseSize + 1),
                 &decoded, &error),
             "oversized response was accepted")) {
    return false;
  }

  response.flags = 1U << 31;
  if (!Check(!protocol::EncodeResponse(response, &encoded, &error),
             "unknown response flag was encoded")) {
    return false;
  }
  response.flags = 0;
  response.output.assign(protocol::kMaxResponseSize, 'o');
  return Check(!protocol::EncodeResponse(response, &encoded, &error),
               "oversized response payload was encoded");
}

} // namespace

int main() {
  return TestRequestGoldenAndRoundTrip() &&
                 TestRequestBoundsAndMalformedFrames() &&
                 TestCodecLeavesCommandPolicyToConsumers() &&
                 TestEmptyPayloadRoundTrip() &&
                 TestResponseGoldenAndRoundTrip() &&
                 TestResponseBoundsAndMalformedFrames()
             ? 0
             : 1;
}

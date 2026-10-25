// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <chrono>
#include <condition_variable>
#include <cstddef>
#include <cstdint>
#include <filesystem>
#include <mutex>
#include <optional>
#include <stdexcept>
#include <string>
#include <vector>

#include "cancellation.hpp"

namespace snapshot::pagebroker {
inline constexpr char kHttpsEndpointPrefix[] = "https://";
inline constexpr char kHttpEndpointPrefix[] = "http://";
inline constexpr char kAwsCaBundleEnv[] = "AWS_CA_BUNDLE";

// Connection settings shared by rclone clients and native restore configuration.
// Empty endpoint uses AWS S3; an empty region uses the startup environment or client default.
struct S3Connection {
  std::string region;
  std::string endpoint;
  bool use_virtual_addressing = true;
  std::string ca_file;
  // Omit all three to read the startup environment. Never log these values.
  std::string access_key_id;
  std::string secret_access_key;
  std::string session_token;

  // Checks text, endpoint scheme and credential completeness without network I/O.
  void Validate() const;
  bool operator==(const S3Connection&) const = default;
};

// Capture explicit or environment credentials once at startup. Missing or incomplete
// credentials fail; profiles and provider discovery must be resolved externally.
S3Connection ResolveS3Credentials(S3Connection connection);

// Identifies one object using a bucket and its literal key, rather than a URL.
struct S3ObjectLocation {
  std::string bucket;
  std::string key;

  // Rejects unsupported bucket names, empty or oversized keys, NUL and line breaks.
  void Validate() const;
};

// One payload file relative to the caller-owned, immutable source tree.
struct S3UploadFile {
  std::filesystem::path relative_path;
  std::uint64_t size_bytes;
};

// Bounds resources shared by concurrent calls to one S3Client and the duration
// and retries of its requests. rclone owns multipart scheduling and buffering.
struct S3Limits {
  std::uint64_t part_size = 16 * 1024 * 1024;
  // Active buffer budget split equally between concurrent rclone processes.
  // Each share must fit one part plus rclone's allocation padding.
  // Idle buffer caches, process overhead, metadata, and file lists are additional.
  std::uint64_t buffer_budget = 1024ULL * 1024 * 1024;
  // Concurrent CLI operations. Each payload batch transfers one file at a time.
  unsigned active_files = 2;
  // Additional attempts at each configurable rclone layer, not an HTTP cap.
  // The S3 backend also has one fixed pacer retry, even when this is zero.
  unsigned request_retries = 3;
  std::chrono::milliseconds connect_timeout{5000};
  std::chrono::milliseconds request_timeout{60000};

  // Rejects inconsistent part/buffer sizes, zero concurrency and invalid timeouts.
  void Validate() const;
  // Checks the configured part count without allocating file-sized resources.
  void ValidateFileSize(std::uint64_t size) const;
  bool operator==(const S3Limits&) const = default;
};

// Exposes HTTP status when available, without retaining server text or credentials.
// A failed write may have completed remotely; callers must confirm publication.
class S3Error : public std::runtime_error {
 public:
  explicit S3Error(int status = 0) : std::runtime_error("S3 operation failed"), http_status(status) {}
  int http_status;
};

// Reports invalid or inconsistent object sizes in otherwise successful reads.
class S3ResponseError : public std::runtime_error {
 public:
  using std::runtime_error::runtime_error;
};

// Performs bounded rclone uploads and small metadata requests. Payload restores
// remain with Model Streamer; Get is intended for bounded metadata objects.
// Every call requires a caller deadline, including time spent waiting for admission.
// Cancellation terminates and reaps the child, with up to two seconds before SIGKILL.
// Forced termination cannot guarantee multipart abort; configure bucket lifecycle cleanup.
// Keep the client alive until all calls return; concurrent calls share resource limits.
class S3Client {
 public:
  // Requires the resolved startup credential snapshot; never discovers credentials.
  explicit S3Client(S3Connection connection, S3Limits limits = {});
  S3Client(const S3Client&) = delete;
  S3Client& operator=(const S3Client&) = delete;

  // Upload only the listed files, preserving paths beneath destination_prefix.
  // Callers must keep the source tree unchanged until return. Concurrent writers
  // to the same source tree or destination prefix are unsupported.
  void UploadFiles(const std::filesystem::path& source_root, const S3ObjectLocation& destination_prefix,
                   const std::vector<S3UploadFile>& files, Cancellation cancellation);
  // Returns the object size, or nullopt for a missing object; other failures throw.
  std::optional<std::uint64_t> Head(const S3ObjectLocation& object, Cancellation cancellation);
  // Returns at most max_bytes after checking HEAD/GET sizes, or returns nullopt
  // for a missing object. Oversized or inconsistent responses throw S3ResponseError.
  std::optional<std::string> Get(const S3ObjectLocation& object, std::size_t max_bytes, Cancellation cancellation);
  // Metadata writes are create-only; a lost response requires caller confirmation.
  // Rejects input larger than max_bytes before sending a request.
  void Put(const S3ObjectLocation& object, const std::string& bytes, std::size_t max_bytes, Cancellation cancellation);

 private:
  // A permit spans preparation, execution and cleanup of one admitted operation.
  struct Operation;
  std::string Execute(Operation& operation, std::vector<std::string> command, int input, std::size_t output_limit);

  const S3Limits limits_;
  std::vector<std::string> environment_;
  std::vector<std::string> arguments_;
  std::mutex mutex_;
  std::condition_variable_any changed_;
  unsigned active_ = 0;
};
} // namespace snapshot::pagebroker

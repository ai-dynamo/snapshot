// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <filesystem>
#include <memory>
#include <optional>
#include <stdexcept>
#include <string>

#include "utils/sha256.hpp"
#include "transfer/transfer_control.hpp"

namespace snapshot::pagebroker {
// Identifies one object using a bucket and its literal key, rather than a URL.
struct S3ObjectLocation {
  std::string bucket;
  std::string key;

  // Rejects unsupported bucket names, empty or oversized keys, NUL and line breaks.
  void Validate() const;
};

// Connection settings shared by SDK clients and native restore configuration.
// Empty endpoint/region values leave discovery to the underlying client.
struct S3Connection {
  std::string region;
  std::string endpoint;
  bool use_virtual_addressing = true;
  std::string ca_file;
  // Omit all three to use the SDK provider chain. Never log these values.
  std::string access_key_id;
  std::string secret_access_key;
  std::string session_token;

  // Checks text, endpoint scheme and credential completeness without network I/O.
  void Validate() const;
  // Compares all settings, including credentials, for configuration consistency.
  bool operator==(const S3Connection&) const = default;
};

// Resolve external AWS providers once; callers explicitly restart for new credentials.
// Returns explicit credentials unchanged, or fails if the provider chain is empty.
S3Connection ResolveS3Credentials(S3Connection connection);

// Bounds resources shared by concurrent calls to one S3Client and the duration
// and retries of its requests. Upload parts consume fixed-size buffer slots.
struct S3Limits {
  std::uint64_t part_size = 16 * 1024 * 1024;
  std::uint64_t buffer_budget = 128 * 1024 * 1024;
  // Caps request concurrency separately from admitted files and buffered parts.
  unsigned workers = 4;
  unsigned active_files = 2;
  unsigned request_retries = 3;
  std::chrono::milliseconds connect_timeout{5000};
  std::chrono::milliseconds request_timeout{60000};
  // Cancellation starts at this deadline; draining and cleanup may take longer.
  std::chrono::milliseconds operation_timeout = std::chrono::hours(2);
  // Bounds multipart abort-and-check attempts after a failed upload.
  unsigned cleanup_rounds = 2;

  // Rejects inconsistent part/buffer sizes, zero concurrency and invalid timeouts.
  void Validate() const;
  // Checks the configured part count without allocating file-sized resources.
  void ValidateFileSize(std::uint64_t size) const;
  // Compares the complete resource and timeout policy.
  bool operator==(const S3Limits&) const = default;
};

// Returned after a completed upload; the digest covers the source payload bytes.
struct S3UploadResult {
  S3ObjectLocation destination;
  std::uint64_t size_bytes;
  utils::Sha256Digest sha256;
};

// Per-call stop conditions, combined with the client's operation timeout.
struct S3UploadOptions {
  TransferControl control;
};

// Identifies the failed SDK operation without retaining its response body or
// credentials. HTTP status and request ID are populated when available.
struct S3RequestFailure {
  std::string operation;
  std::string code;
  int http_status = 0;
  std::string request_id;
};

// Describes this attempt, not whether an older object exists at the same key.
enum class S3RemoteState { NO_WRITES_ISSUED, NOT_COMPLETED, COMPLETED, UNKNOWN };
// Whether multipart cleanup was needed and whether its completion was verified.
enum class S3CleanupState { NOT_NEEDED, CONFIRMED, UNCONFIRMED };

// Preserves the primary failure independently of remote outcome and cleanup.
// An uncertain outcome must not be treated as proof that no object was written.
class S3Error : public std::runtime_error {
 public:
  // Records the primary operation/code; upload handling adds outcome details.
  explicit S3Error(S3RequestFailure failure);

  S3RequestFailure primary;
  S3ObjectLocation destination;
  S3RemoteState remote_state = S3RemoteState::NO_WRITES_ISSUED;
  std::string upload_id;
  S3CleanupState cleanup_state = S3CleanupState::NOT_NEEDED;
  std::optional<S3RequestFailure> cleanup_failure;
};

// Reports invalid or inconsistent object sizes in otherwise successful reads.
class S3ResponseError : public std::runtime_error {
 public:
  // Uses the caller's size-validation diagnostic as the exception message.
  using std::runtime_error::runtime_error;
};

// Performs bounded SDK uploads and small metadata requests. Payload restores
// remain with Model Streamer; Get is intended for bounded metadata objects.
// Keep the client alive until all calls return; concurrent calls share resource limits.
class S3Client {
 public:
  // Validates settings and creates the SDK clients and shared resource controls.
  explicit S3Client(S3Connection connection, S3Limits limits = {});
  // Releases SDK state after callers have finished all operations.
  ~S3Client() noexcept;
  // Keeps SDK state and its resource limits owned by one client instance.
  S3Client(const S3Client&) = delete;
  S3Client& operator=(const S3Client&) = delete;

  // Source files must remain immutable; overwrites require exclusive destination keys.
  // Streams regular-file contents, using multipart upload when needed, and
  // returns their size and SHA-256 digest only after completion is confirmed.
  S3UploadResult UploadFile(const std::filesystem::path& source, const S3ObjectLocation& destination,
                           const S3UploadOptions& options = {});
  // Returns the object size, or nullopt for a missing object; other failures throw.
  std::optional<std::uint64_t> Head(const S3ObjectLocation& object, TransferControl control = {});
  // Returns at most max_bytes after checking HEAD/GET sizes, or returns nullopt
  // for a missing object. Oversized or inconsistent responses throw S3ResponseError.
  std::optional<std::string> Get(const S3ObjectLocation& object, std::size_t max_bytes, TransferControl control = {});
  // Metadata writes are create-only; a lost response requires caller confirmation.
  // Rejects input larger than max_bytes before sending a request.
  void Put(const S3ObjectLocation& object, const std::string& bytes, std::size_t max_bytes, TransferControl control = {});

 private:
  // Hides SDK types, request scheduling and shared admission/buffer accounting.
  struct Impl;
  std::unique_ptr<Impl> impl_;
};
}  // namespace snapshot::pagebroker

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <memory>
#include <mutex>
#include <string>
#include <vector>

#include "transfer/restore_plan.hpp"
#include "s3_config.hpp"

namespace snapshot::pagebroker {
// Maps one regular file beneath the source directory to its remote object.
struct S3CheckpointFile {
  Path relative_path;
  S3ObjectLocation destination;
};

// Describes a complete local tree and explicit destinations for its file payloads.
struct S3CheckpointUploadPlan {
  Path source_directory;
  // Exactly one caller-owned, exclusive bucket/key per regular file.
  std::vector<S3CheckpointFile> files;
};

// Direct SDK uploads using immutable startup settings. Explicit file mappings
// supply their own bucket/key. Native restore sessions stay in the transfer engine.
class S3StorageBackend {
 public:
  // Validates and retains startup settings; the SDK client is created on demand.
  explicit S3StorageBackend(S3Config config);
  // Keeps the lazy client and its shared resource limits owned by this backend.
  S3StorageBackend(const S3StorageBackend&) = delete;
  S3StorageBackend& operator=(const S3StorageBackend&) = delete;

  // Immutable snapshot of the startup settings supplied by the engine.
  const S3Config& config() const;
  // Validates every explicit file mapping, uploads payloads and returns their
  // restore plan with digests; this path does not publish a checkpoint index.
  // Keep the source immutable and the backend alive until calls finish.
  // Failures leave completed remote objects intact.
  RestorePlan UploadCheckpoint(const S3CheckpointUploadPlan& plan, const S3UploadOptions& options = {}) const;

 private:
  // Initializes one shared SDK client safely on the first operation that needs it.
  S3Client& Client() const;
  // Uploads planned files in turn, verifying their sizes and recording digests.
  RestorePlan UploadFiles(const Path& source, RestorePlan plan, const S3UploadOptions& options) const;

  const S3Config config_;
  mutable std::once_flag client_once_;
  mutable std::unique_ptr<S3Client> client_;
};
}  // namespace snapshot::pagebroker

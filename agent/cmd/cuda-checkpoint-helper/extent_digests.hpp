/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#pragma once

#include "storage_manifest.hpp"

namespace cuda_checkpoint_storage {

constexpr const char* kExtentDigestsName = "sha256.txt";

// A bounded, versioned sidecar binds digests to each manifest extent's UUID,
// length, and filename. It is optional; requesting verification requires it.
// Write requires the same exclusive fresh directory ownership as WriteManifest.
bool WriteExtentDigests(const std::filesystem::path& directory,
                        const std::vector<ManifestExtent>& manifest,
                        const std::vector<std::string>& digests,
                        std::string* error);
bool ReadExtentDigests(const std::filesystem::path& directory,
                       const std::vector<ManifestExtent>& manifest,
                       std::vector<std::string>* digests, std::string* error);

// Optional checksum metadata, indexed in manifest extent order.
// Checkpoint fills a pre-sized digest vector. Restore verifies every extent.
bool ApplyOrVerifyExtentDigests(bool checkpoint,
                               const std::vector<TransferJob> &jobs,
                               const std::vector<std::string> &digests,
                               std::vector<std::string> *extent_digests,
                               std::string *error);

} // namespace cuda_checkpoint_storage

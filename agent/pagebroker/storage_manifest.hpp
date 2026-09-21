/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#pragma once

#include <array>
#include <cstddef>
#include <filesystem>
#include <string>
#include <string_view>
#include <vector>

namespace cuda_checkpoint_storage {

constexpr const char *kManifestName = "manifest.txt";

struct ManifestExtent {
  std::string source_uuid;
  size_t size = 0;
  std::string filename;
};

struct DeviceExtent {
  std::string uuid;
  size_t size = 0;
};

struct DevicePair {
  std::string source_uuid;
  std::string destination_uuid;
};

struct TransferJob {
  size_t device_index = 0;
  size_t extent_index = 0;
};

bool ParseGPUUUID(std::string_view value,
                  std::array<unsigned char, 16> *bytes_out);
std::string FormatGPUUUID(const std::array<unsigned char, 16> &bytes);
bool CanonicalizeGPUUUID(std::string_view value, std::string *canonical_out);

std::string DeviceFilename(size_t index);

bool BuildCheckpointManifest(const std::vector<DeviceExtent> &devices,
                             std::vector<ManifestExtent> *extents,
                             std::string *error);

// Builds jobs in current local-device order. A nonempty device map is
// interpreted as source->destination and reversed to recover each destination's
// source UUID. Extra pairs are permitted because a process may export only a
// subset of the GPUs assigned to its container.
bool BuildTransferJobs(const std::vector<ManifestExtent> &extents,
                       const std::vector<DeviceExtent> &devices,
                       const std::vector<DevicePair> &device_pairs,
                       std::vector<TransferJob> *jobs, std::string *error);

// WriteManifest requires a fresh participant directory. The broker publishes
// the enclosing checkpoint transaction after all participants complete.
// Manifest lifecycle functions require the caller to own the participant
// directory exclusively for the operation. PageBroker provides one trusted
// directory per CUDA participant beneath its transaction-exclusive staging
// root; callers must not run concurrent lifecycle operations against the same
// participant directory.
bool WriteManifest(const std::filesystem::path &directory,
                   const std::vector<ManifestExtent> &extents,
                   std::string *error);
bool ReadManifest(const std::filesystem::path &directory,
                  std::vector<ManifestExtent> *extents, std::string *error);
bool ValidateExtentFiles(const std::filesystem::path &directory,
                         const std::vector<ManifestExtent> &extents,
                         std::string *error);

} // namespace cuda_checkpoint_storage

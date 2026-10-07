/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#pragma once

#include <array>
#include <cstddef>
#include <filesystem>
#include <string>
#include <string_view>
#include <vector>

namespace snapshot::pagebroker::gpu {
inline constexpr char kDataDirectory[] = "gpu";
}

namespace snapshot::pagebroker::gpu::storage {

constexpr const char *kManifestName = "manifest.txt";

struct GpuDataFile {
  std::string source_uuid;
  size_t size = 0;
  std::string filename;
};

struct GpuDataRegion {
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

bool BuildCheckpointManifest(const std::vector<GpuDataRegion> &devices,
                             std::vector<GpuDataFile> *extents,
                             std::string *error);

// Build jobs in local device order. Use the source-to-destination map to find
// each destination's source UUID. Allow extra pairs because a process may use
// only some of its container's GPUs.
bool BuildTransferJobs(const std::vector<GpuDataFile> &extents,
                       const std::vector<GpuDataRegion> &devices,
                       const std::vector<DevicePair> &device_pairs,
                       std::vector<TransferJob> *jobs, std::string *error);

// WriteManifest needs a new directory that only this operation uses. Publish
// the checkpoint only after all participants complete.
bool WriteManifest(const std::filesystem::path &directory,
                   const std::vector<GpuDataFile> &extents,
                   std::string *error);
bool ReadManifest(const std::filesystem::path &directory,
                  std::vector<GpuDataFile> *extents, std::string *error);
bool ValidateExtentFiles(const std::filesystem::path &directory,
                         const std::vector<GpuDataFile> &extents,
                         std::string *error);

} // namespace snapshot::pagebroker::gpu::storage

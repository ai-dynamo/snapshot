/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#include "storage_manifest.h"

#include <unistd.h>

#include <cstdlib>
#include <array>
#include <filesystem>
#include <fstream>
#include <iostream>
#include <string>
#include <vector>

namespace storage = cuda_checkpoint_storage;

namespace {

constexpr const char *kSourceA = "GPU-00000000-0000-0000-0000-00000000000a";
constexpr const char *kSourceB = "GPU-00000000-0000-0000-0000-00000000000b";
constexpr const char *kSourceFallback =
    "GPU-00000000-0000-0000-0000-00000000000c";
constexpr const char *kDestinationA =
    "GPU-10000000-0000-0000-0000-00000000000a";
constexpr const char *kDestinationB =
    "GPU-10000000-0000-0000-0000-00000000000b";
constexpr const char *kDestinationFallback =
    "GPU-10000000-0000-0000-0000-00000000000c";
constexpr const char *kDigestA =
    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
constexpr const char *kDigestB =
    "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";

bool Check(bool condition, const std::string &message) {
  if (!condition) {
    std::cerr << message << "\n";
  }
  return condition;
}

bool TestGPUUUIDParsing() {
  std::array<unsigned char, 16> parsed{};
  std::string canonical;
  return Check(storage::ParseGPUUUID(kSourceA, &parsed),
               "canonical GPU UUID was rejected") &&
         Check(storage::FormatGPUUUID(parsed) == kSourceA,
               "GPU UUID did not round-trip") &&
         Check(storage::CanonicalizeGPUUUID(
                   "00000000-0000-0000-0000-00000000000A", &canonical) &&
                   canonical == kSourceA,
               "bare uppercase GPU UUID was not canonicalized") &&
         Check(!storage::ParseGPUUUID(
                   "GPU-00000000-0000-0000-0000-00000000000g", &parsed),
               "non-hex GPU UUID was accepted") &&
         Check(!storage::ParseGPUUUID("GPU-0000", &parsed),
               "short GPU UUID was accepted");
}

bool TestEqualSizeNonOrderPreservingMap() {
  const std::vector<storage::ManifestExtent> extents{
      {kSourceA, 4096, storage::DeviceFilename(0), ""},
      {kSourceB, 4096, storage::DeviceFilename(1), ""},
  };
  // CUDA returns destination B first. Equal sizes must not permit an
  // index-based A/B swap.
  const std::vector<storage::DeviceExtent> destinations{
      {kDestinationB, 4096},
      {kDestinationA, 4096},
  };
  const std::vector<storage::DevicePair> pairs{
      {kSourceA, kDestinationA},
      {kSourceB, kDestinationB},
      // This GPU is assigned to the container but is not exported by this
      // process. Its explicit fallback pair must not invalidate the subset.
      {kSourceFallback, kDestinationFallback},
  };

  std::vector<storage::TransferJob> jobs;
  std::string error;
  return Check(storage::BuildTransferJobs(extents, destinations, pairs, &jobs,
                                          &error),
               error) &&
         Check(jobs.size() == 2, "expected two transfer jobs") &&
         Check(jobs[0].device_index == 0 && jobs[0].extent_index == 1,
               "destination B was not matched to source B's deterministic "
               "file") &&
         Check(
             jobs[1].device_index == 1 && jobs[1].extent_index == 0,
             "destination A was not matched to source A's deterministic file");
}

bool TestEmptyV3Manifest() {
  char path[] = "/tmp/cuda-storage-manifest-test-XXXXXX";
  const char *directory = mkdtemp(path);
  if (!Check(directory != nullptr, "mkdtemp failed")) {
    return false;
  }

  std::string error;
  std::vector<storage::ManifestExtent> loaded;
  std::vector<storage::TransferJob> jobs;
  const bool result =
      Check(storage::WriteManifest(directory, {}, &error), error) &&
      Check(storage::ReadManifest(directory, &loaded, &error), error) &&
      Check(loaded.empty(), "empty v3 manifest did not round-trip") &&
      Check(storage::BuildTransferJobs(loaded, {}, {}, &jobs, &error), error) &&
      Check(jobs.empty(), "zero-device restore produced transfer jobs");
  std::error_code ignored;
  std::filesystem::remove_all(directory, ignored);
  return result;
}

bool TestNonemptyV3ManifestRoundTrip() {
  char path[] = "/tmp/cuda-storage-manifest-roundtrip-test-XXXXXX";
  const char *directory = mkdtemp(path);
  if (!Check(directory != nullptr, "mkdtemp failed")) {
    return false;
  }
  const std::vector<storage::ManifestExtent> extents{
      {kSourceA, 4096, storage::DeviceFilename(0), kDigestA},
      {kSourceB, 8192, storage::DeviceFilename(1), kDigestB},
  };

  std::string error;
  std::vector<storage::ManifestExtent> loaded;
  const auto manifest_path =
      std::filesystem::path(directory) / storage::kManifestName;
  const bool result =
      Check(storage::WriteManifest(directory, extents, &error), error) &&
      Check((std::filesystem::status(manifest_path).permissions() &
             std::filesystem::perms::all) ==
                (std::filesystem::perms::owner_read |
                 std::filesystem::perms::owner_write),
            "committed manifest permissions are not 0600") &&
      Check(storage::ReadManifest(directory, &loaded, &error), error) &&
      Check(loaded.size() == 2 && loaded[0].source_uuid == kSourceA &&
                loaded[0].size == 4096 &&
                loaded[0].filename == "device-0000.bin" &&
                loaded[0].sha256 == kDigestA &&
                loaded[1].source_uuid == kSourceB && loaded[1].size == 8192 &&
                loaded[1].filename == "device-0001.bin" &&
                loaded[1].sha256 == kDigestB,
            "nonempty v3 manifest did not preserve UUID, size, filename, and digest");
  std::error_code ignored;
  std::filesystem::remove_all(directory, ignored);
  return result;
}

bool TestV1Rejected() {
  char path[] = "/tmp/cuda-storage-manifest-v1-test-XXXXXX";
  const char *directory = mkdtemp(path);
  if (!Check(directory != nullptr, "mkdtemp failed")) {
    return false;
  }
  {
    std::ofstream output(std::filesystem::path(directory) /
                         storage::kManifestName);
    output << "version 1\n"
              "device_count 1\n"
              "device 0 4096 device-0000.bin\n";
  }

  std::vector<storage::ManifestExtent> extents;
  std::string error;
  const bool result = Check(!storage::ReadManifest(directory, &extents, &error),
                            "unsafe v1 manifest was accepted") &&
                      Check(error.find("without extent digests") !=
                                std::string::npos,
                            "v1 rejection did not identify missing integrity metadata");
  std::error_code ignored;
  std::filesystem::remove_all(directory, ignored);
  return result;
}

bool TestV2RejectedWithoutDigest() {
  char path[] = "/tmp/cuda-storage-manifest-v2-test-XXXXXX";
  const char *directory = mkdtemp(path);
  if (!Check(directory != nullptr, "mkdtemp failed")) {
    return false;
  }
  {
    std::ofstream output(std::filesystem::path(directory) /
                         storage::kManifestName);
    output << "version 2\n"
              "device_count 1\n"
              "device 0 "
           << kSourceA << " 4096 device-0000.bin\n";
  }

  std::vector<storage::ManifestExtent> extents;
  std::string error;
  const bool result =
      Check(!storage::ReadManifest(directory, &extents, &error),
            "unsafe v2 manifest without digest was accepted") &&
      Check(error.find("without extent digests") != std::string::npos,
            "v2 rejection did not identify missing integrity metadata");
  std::error_code ignored;
  std::filesystem::remove_all(directory, ignored);
  return result;
}

bool TestManifestSymlinkRejected() {
  char path[] = "/tmp/cuda-storage-manifest-symlink-test-XXXXXX";
  const char *directory = mkdtemp(path);
  if (!Check(directory != nullptr, "mkdtemp failed")) {
    return false;
  }
  const auto target = std::filesystem::path(directory) / "target.txt";
  const auto manifest =
      std::filesystem::path(directory) / storage::kManifestName;
  {
    std::ofstream output(target);
    output << "version 3\ndevice_count 0\n";
  }
  const bool linked = symlink(target.c_str(), manifest.c_str()) == 0;

  std::vector<storage::ManifestExtent> extents;
  std::string error;
  const bool rejected =
      linked && !storage::ReadManifest(directory, &extents, &error);
  std::error_code ignored;
  std::filesystem::remove_all(directory, ignored);
  return Check(linked, "failed to create manifest symlink fixture") &&
         Check(rejected, "helper manifest symlink was accepted");
}

bool TestUnconsumedExtentRejected() {
  const std::vector<storage::ManifestExtent> extents{
      {kSourceA, 4096, storage::DeviceFilename(0), ""},
      {kSourceB, 4096, storage::DeviceFilename(1), ""},
  };
  const std::vector<storage::DeviceExtent> destinations{{kSourceA, 4096}};
  std::vector<storage::TransferJob> jobs;
  std::string error;
  return Check(
      !storage::BuildTransferJobs(extents, destinations, {}, &jobs, &error),
      "restore accepted an unconsumed saved extent");
}

bool TestUnsafeMappingsRejected() {
  const std::vector<storage::ManifestExtent> extents{
      {kSourceA, 4096, storage::DeviceFilename(0), ""},
      {kSourceB, 4096, storage::DeviceFilename(1), ""},
  };
  std::vector<storage::TransferJob> jobs;
  std::string error;

  if (!Check(!storage::BuildTransferJobs(
                 extents, {{kDestinationA, 4096}, {kDestinationB, 4096}},
                 {{kSourceA, kDestinationA}}, &jobs, &error),
             "restore accepted a destination missing from the device map")) {
    return false;
  }
  if (!Check(!storage::BuildTransferJobs(
                 extents, {{kDestinationA, 4096}, {kDestinationB, 4096}},
                 {{kSourceA, kDestinationA}, {kSourceB, kDestinationA}}, &jobs,
                 &error),
             "restore accepted an ambiguous destination UUID")) {
    return false;
  }
  if (!Check(!storage::BuildTransferJobs(
                 extents, {{kDestinationA, 4096}, {kDestinationB, 8192}},
                 {{kSourceA, kDestinationA}, {kSourceB, kDestinationB}}, &jobs,
                 &error),
             "restore accepted a UUID-matched extent with the wrong size")) {
    return false;
  }
  if (!Check(!storage::BuildTransferJobs(
                 {{kSourceA, 4096, storage::DeviceFilename(0), ""},
                  {kSourceA, 4096, storage::DeviceFilename(1), ""}},
                 {{kSourceA, 4096}, {kSourceB, 4096}}, {}, &jobs, &error),
             "restore accepted duplicate saved source UUIDs")) {
    return false;
  }
  return Check(!storage::BuildTransferJobs(extents,
                                           {{kSourceA, 4096}, {kSourceA, 4096}},
                                           {}, &jobs, &error),
               "restore accepted duplicate destination UUIDs");
}

bool TestDuplicateCheckpointUUIDRejected() {
  std::vector<storage::ManifestExtent> extents;
  std::string error;
  return Check(!storage::BuildCheckpointManifest(
                   {{kSourceA, 4096}, {kSourceA, 4096}}, &extents, &error),
               "checkpoint accepted duplicate source UUIDs");
}

bool TestExtentDigestApplyAndSameSizeCorruptionRejection() {
  std::vector<storage::ManifestExtent> extents{
      {kSourceA, 4096, storage::DeviceFilename(0), ""},
      {kSourceB, 4096, storage::DeviceFilename(1), ""},
  };
  const std::vector<storage::TransferJob> jobs{{0, 1}, {1, 0}};
  std::string error;
  if (!Check(storage::ApplyOrVerifyExtentDigests(
                 true, jobs, {kDigestB, kDigestA}, &extents, &error),
             error) ||
      !Check(extents[0].sha256 == kDigestA &&
                 extents[1].sha256 == kDigestB,
             "checkpoint digests were not mapped by extent identity")) {
    return false;
  }
  if (!Check(storage::ApplyOrVerifyExtentDigests(
                 false, jobs, {kDigestB, kDigestA}, &extents, &error),
             error)) {
    return false;
  }
  return Check(!storage::ApplyOrVerifyExtentDigests(
                   false, jobs, {kDigestB, std::string(64, 'c')}, &extents,
                   &error),
               "same-size extent corruption was accepted") &&
         Check(error.find("SHA-256 mismatch") != std::string::npos,
               "corruption rejection did not report a digest mismatch");
}

bool TestWrongDeterministicFilenameRejected() {
  std::vector<storage::TransferJob> jobs;
  std::string error;
  return Check(!storage::BuildTransferJobs(
                   {{kSourceA, 4096, "device-0001.bin", ""}},
                   {{kSourceA, 4096}}, {}, &jobs, &error),
               "manifest accepted an extent with the wrong deterministic "
               "filename");
}

bool TestValidateExtentFiles() {
  char path[] = "/tmp/cuda-storage-extent-test-XXXXXX";
  const char *directory = mkdtemp(path);
  if (!Check(directory != nullptr, "mkdtemp failed")) {
    return false;
  }
  const std::filesystem::path extent_path =
      std::filesystem::path(directory) / storage::DeviceFilename(0);
  {
    std::ofstream extent(extent_path, std::ios::binary);
    extent << "bad";
  }
  const std::vector<storage::ManifestExtent> extents{
      {kSourceA, 4, storage::DeviceFilename(0), ""},
  };
  std::string error;
  const bool rejected_wrong_size =
      !storage::ValidateExtentFiles(directory, extents, &error);
  const bool resized = truncate(extent_path.c_str(), 4) == 0;
  const bool accepted_exact_size =
      resized && storage::ValidateExtentFiles(directory, extents, &error);
  std::error_code ignored;
  std::filesystem::remove_all(directory, ignored);
  return Check(rejected_wrong_size,
               "ValidateExtentFiles accepted an incorrect extent size") &&
         Check(resized, "failed to resize extent fixture") &&
         Check(accepted_exact_size,
               "ValidateExtentFiles rejected the exact extent size");
}

bool TestRemoveManifest() {
  char path[] = "/tmp/cuda-storage-remove-test-XXXXXX";
  const char *directory = mkdtemp(path);
  if (!Check(directory != nullptr, "mkdtemp failed")) {
    return false;
  }
  const auto manifest = std::filesystem::path(directory) / "manifest.txt";
  const auto temporary = std::filesystem::path(directory) /
                         storage::kLegacyTemporaryManifestName;
  const auto unique_temporary = std::filesystem::path(directory) /
                                (std::string(storage::kTemporaryManifestPrefix) +
                                 "123.456");
  {
    std::ofstream(manifest) << "manifest";
    std::ofstream(temporary) << "temporary";
    std::ofstream(unique_temporary) << "temporary";
  }
  std::string error;
  const bool first = storage::RemoveManifest(directory, &error);
  const bool removed = !std::filesystem::exists(manifest) &&
                       !std::filesystem::exists(temporary) &&
                       !std::filesystem::exists(unique_temporary);
  const bool second = storage::RemoveManifest(directory, &error);
  std::error_code ignored;
  std::filesystem::remove_all(directory, ignored);
  return Check(first, error) && Check(removed, "manifest files remain") &&
         Check(second, "repeated RemoveManifest failed");
}

bool TestStaleTemporaryManifestDoesNotBlockWrite() {
  char path[] = "/tmp/cuda-storage-stale-temporary-test-XXXXXX";
  const char *directory = mkdtemp(path);
  if (!Check(directory != nullptr, "mkdtemp failed")) {
    return false;
  }
  const auto stale = std::filesystem::path(directory) /
                     (std::string(storage::kTemporaryManifestPrefix) +
                      "111.222");
  std::ofstream(stale) << "stale";
  std::string error;
  const bool wrote = storage::WriteManifest(directory, {}, &error);
  const bool cleaned = !std::filesystem::exists(stale);
  std::error_code ignored;
  std::filesystem::remove_all(directory, ignored);
  return Check(wrote, error) &&
         Check(cleaned, "stale temporary manifest was not removed");
}

} // namespace

int main() {
  if (!TestGPUUUIDParsing() || !TestEqualSizeNonOrderPreservingMap() ||
      !TestEmptyV3Manifest() ||
      !TestNonemptyV3ManifestRoundTrip() || !TestV1Rejected() ||
      !TestV2RejectedWithoutDigest() || !TestManifestSymlinkRejected() ||
      !TestUnconsumedExtentRejected() || !TestUnsafeMappingsRejected() ||
      !TestDuplicateCheckpointUUIDRejected() ||
      !TestExtentDigestApplyAndSameSizeCorruptionRejection() ||
      !TestWrongDeterministicFilenameRejected() ||
      !TestValidateExtentFiles() || !TestRemoveManifest() ||
      !TestStaleTemporaryManifestDoesNotBlockWrite()) {
    return 1;
  }
  std::cout << "cuda checkpoint storage manifest tests passed\n";
  return 0;
}

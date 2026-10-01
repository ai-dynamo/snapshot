/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#include "extent_digests.hpp"

#include <iostream>
#include <filesystem>
#include <fstream>
#include <unistd.h>
#include <sys/stat.h>

namespace storage = cuda_checkpoint_storage;
namespace {
const std::string kDigestA(64, 'a');
const std::string kDigestB(64, 'b');
bool Check(bool result, const std::string &message) {
  if (!result) std::cerr << message << '\n';
  return result;
}

bool TestExtentDigestApplyAndSameSizeCorruptionRejection() {
  std::vector<std::string> extents(2);
  const std::vector<storage::TransferJob> jobs{{0, 1}, {1, 0}};
  std::string error;
  if (!Check(storage::ApplyOrVerifyExtentDigests(
                 true, jobs, {kDigestB, kDigestA}, &extents, &error),
             error) ||
      !Check(extents[0] == kDigestA &&
                 extents[1] == kDigestB,
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

bool TestDigestCoverage() {
  std::vector<std::string> expected{kDigestA, kDigestB};
  std::string error;
  return Check(!storage::ApplyOrVerifyExtentDigests(
                   false, {{0, 0}}, {kDigestA}, &expected, &error),
               "verification accepted incomplete coverage") &&
         Check(!storage::ApplyOrVerifyExtentDigests(
                   false, {{0, 0}, {1, 0}}, {kDigestA, kDigestA}, &expected, &error),
               "verification accepted duplicate extent indices") &&
         Check(!storage::ApplyOrVerifyExtentDigests(
                   false, {{0, 0}, {1, 1}}, {kDigestA}, &expected, &error),
               "verification accepted a missing transfer digest") &&
         Check(!storage::ApplyOrVerifyExtentDigests(
                   false, {{0, 0}, {1, 1}}, {kDigestA, ""}, &expected, &error),
               "verification accepted an absent SHA-256 value");
}

bool TestSidecarIdentityAndBoundedParsing() {
  char pattern[] = "/tmp/pagebroker-digests-XXXXXX";
  const char* path = mkdtemp(pattern);
  if (!Check(path != nullptr, "create sidecar test directory")) return false;
  const std::filesystem::path directory(path);
  struct Cleanup {
    std::filesystem::path directory;
    ~Cleanup() { std::filesystem::remove_all(directory); }
  } cleanup{directory};
  std::vector<storage::ManifestExtent> manifest{
      {"GPU-00112233-4455-6677-8899-aabbccddeeff", 4096, storage::DeviceFilename(0)},
      {"GPU-11223344-5566-7788-99aa-bbccddeeff00", 8192, storage::DeviceFilename(1)}};
  const std::vector<std::string> expected{kDigestA, kDigestB};
  std::vector<std::string> actual;
  std::string error;
  if (!Check(!storage::ReadExtentDigests(directory, manifest, &actual, &error),
             "missing sidecar must fail requested verification") ||
      !Check(storage::WriteExtentDigests(directory, manifest, expected, &error), error) ||
      !Check(storage::ReadExtentDigests(directory, manifest, &actual, &error), error) ||
      !Check(actual == expected, "sidecar digest roundtrip differs") ||
      !Check(!storage::WriteExtentDigests(directory, manifest, expected, &error),
             "existing sidecar must not be overwritten")) return false;
  auto mismatched = manifest;
  mismatched[0].size += 4096;
  if (!Check(!storage::ReadExtentDigests(directory, mismatched, &actual, &error),
             "sidecar accepted incorrect extent length")) return false;
  mismatched = manifest;
  std::swap(mismatched[0].source_uuid, mismatched[1].source_uuid);
  if (!Check(!storage::ReadExtentDigests(directory, mismatched, &actual, &error),
             "sidecar accepted swapped GPU identities")) return false;
  const auto sidecar = directory / storage::kExtentDigestsName;
  const auto valid = directory / "valid-digests";
  std::filesystem::rename(sidecar, valid);
  std::filesystem::create_symlink(valid, sidecar);
  if (!Check(!storage::ReadExtentDigests(directory, manifest, &actual, &error),
             "sidecar reader followed symlink")) return false;
  std::filesystem::remove(sidecar);
  if (!Check(mkfifo(sidecar.c_str(), 0600) == 0, "create sidecar FIFO")) return false;
  if (!Check(!storage::ReadExtentDigests(directory, manifest, &actual, &error),
             "sidecar reader accepted FIFO")) return false;
  std::filesystem::remove(sidecar);
  std::filesystem::rename(valid, sidecar);
  {
    std::ofstream append(sidecar, std::ios::app);
    append << "trailing data\n";
  }
  if (!Check(!storage::ReadExtentDigests(directory, manifest, &actual, &error),
             "sidecar accepted trailing data")) return false;
  std::filesystem::resize_file(sidecar, 256 * 1024 + 1);
  if (!Check(!storage::ReadExtentDigests(directory, manifest, &actual, &error),
             "sidecar accepted oversized metadata")) return false;
  std::filesystem::remove(sidecar);
  {
    std::ofstream invalid(sidecar);
    invalid << "PAGEBROKER_SHA256_V2\n2\n";
  }
  return Check(!storage::ReadExtentDigests(directory, manifest, &actual, &error),
               "sidecar accepted unknown version");
}
} // namespace

int main() {
  return TestExtentDigestApplyAndSameSizeCorruptionRejection() &&
                 TestDigestCoverage() &&
                 TestSidecarIdentityAndBoundedParsing()
             ? 0
             : 1;
}

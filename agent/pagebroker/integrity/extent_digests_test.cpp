/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#include "extent_digests.hpp"

#include <iostream>

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
} // namespace

int main() {
  return TestExtentDigestApplyAndSameSizeCorruptionRejection() &&
                 TestDigestCoverage()
             ? 0
             : 1;
}

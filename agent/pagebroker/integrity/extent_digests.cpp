/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#include "extent_digests.hpp"
#include "content_digest.hpp"

namespace cuda_checkpoint_storage {

bool ApplyOrVerifyExtentDigests(bool checkpoint,
                                const std::vector<TransferJob> &jobs,
                                const std::vector<std::string> &digests,
                                std::vector<std::string> *extent_digests,
                                std::string *error) {
  if (extent_digests == nullptr || error == nullptr) {
    return false;
  }
  if (jobs.size() != digests.size()) {
    *error = "custom storage digest coverage mismatch";
    return false;
  }
  std::vector<unsigned char> covered(extent_digests->size(), 0);
  for (size_t job_index = 0; job_index < jobs.size(); ++job_index) {
    const size_t extent_index = jobs[job_index].extent_index;
    if (extent_index >= extent_digests->size() || covered[extent_index] != 0 ||
        !IsSHA256Hex(digests[job_index])) {
      *error = "custom storage digest metadata is invalid";
      return false;
    }
    covered[extent_index] = 1;
    auto &expected = (*extent_digests)[extent_index];
    if (checkpoint) {
      expected = digests[job_index];
      continue;
    }
    if (expected != digests[job_index]) {
      *error = "custom storage extent SHA-256 mismatch for " + std::to_string(extent_index);
      return false;
    }
  }
  for (const unsigned char value : covered) {
    if (value == 0) {
      *error = "one or more custom storage extents lack digest coverage";
      return false;
    }
  }
  return true;
}

} // namespace cuda_checkpoint_storage

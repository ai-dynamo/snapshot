/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#pragma once

#include <cstddef>
#include <cstdint>
#include <stdexcept>
#include <string>
#include <utility>
#include <vector>

namespace cuda_checkpoint_operation {

constexpr std::size_t kMaxRestoreBatchTargets = 64;

struct RestoreBatchJobFileIdentity {
  bool present = false;
  std::uint64_t device = 0;
  std::uint64_t inode = 0;

  bool operator==(const RestoreBatchJobFileIdentity &) const = default;
};

struct RestoreBatchJobFileGroup {
  std::string representative;
  RestoreBatchJobFileIdentity identity;
  std::vector<std::size_t> request_indices;
};

// Open without following symlinks and resolve the stable filesystem identity
// used by both PageBroker scheduling and the worker's pre-dispatch recheck.
bool ResolveRestoreBatchJobFileIdentity(
    const std::string &path, RestoreBatchJobFileIdentity *identity,
    std::string *error);

// Resolve every non-empty path before grouping it. Production supplies an
// O_NOFOLLOW/open/fstat resolver; tests can inject stable identities without
// depending on a particular filesystem. Grouping by device and inode makes
// hard-link aliases for one persisted launch job share a preparation window.
template <typename ResolveIdentity>
bool GroupRestoreBatchJobFiles(
    const std::vector<std::string> &job_files, ResolveIdentity &&resolve,
    std::vector<RestoreBatchJobFileGroup> *groups, std::string *error) {
  groups->clear();
  for (std::size_t index = 0; index < job_files.size(); ++index) {
    RestoreBatchJobFileIdentity identity;
    if (!job_files[index].empty() &&
        !resolve(job_files[index], &identity, error)) {
      return false;
    }
    if (!job_files[index].empty() && !identity.present) {
      *error = "job-file identity resolver returned no identity";
      return false;
    }
    auto group = groups->begin();
    for (; group != groups->end(); ++group) {
      if (group->identity == identity)
        break;
    }
    if (group == groups->end()) {
      groups->push_back({.representative = job_files[index],
                         .identity = identity,
                         .request_indices = {index}});
    } else {
      group->request_indices.push_back(index);
    }
  }
  return true;
}

// Batch restore requests preserve the captured process-tree order: parents
// precede children. CUDA operation completion resumes a target, so targets must
// be completed in the opposite order to keep a resumed parent from terminating
// a child whose CUDA operation is still pending.
constexpr std::size_t RestoreBatchCompletionIndex(std::size_t completed,
                                                  std::size_t target_count) {
  return target_count - completed - 1;
}

// Prepare launch-job participants one at a time in captured process-tree order.
// CUDA_CHECKPOINT_JOB_FILE describes the whole launch job, but the CUDA process
// restore API still requires one ordered call per target. The caller releases
// the job-file scope after this short handle-preparation phase, then transfers
// every prepared target's independent extents concurrently.
template <typename Prepare>
std::size_t RunRestoreBatchPreparation(
    std::size_t target_count, Prepare &&prepare,
    std::size_t *first_exception_index = nullptr,
    std::size_t *observed_width = nullptr) {
  if (target_count > kMaxRestoreBatchTargets) {
    throw std::invalid_argument("restore batch exceeds 64 targets");
  }
  if (first_exception_index != nullptr)
    *first_exception_index = target_count;
  if (observed_width != nullptr)
    *observed_width = 0;
  for (std::size_t index = 0; index < target_count; ++index) {
    try {
      if (observed_width != nullptr)
        *observed_width = 1;
      prepare(index);
    } catch (...) {
      if (first_exception_index != nullptr)
        *first_exception_index = index;
      throw;
    }
  }
  return target_count == 0 ? 0 : 1;
}

} // namespace cuda_checkpoint_operation

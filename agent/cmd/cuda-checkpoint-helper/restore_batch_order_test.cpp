/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#include "restore_batch_order.h"

#include <cassert>
#include <map>
#include <stdexcept>
#include <string>
#include <vector>

int main() {
  using cuda_checkpoint_operation::GroupRestoreBatchJobFiles;
  using cuda_checkpoint_operation::kMaxRestoreBatchTargets;
  using cuda_checkpoint_operation::RestoreBatchJobFileGroup;
  using cuda_checkpoint_operation::RestoreBatchJobFileIdentity;
  using cuda_checkpoint_operation::RestoreBatchCompletionIndex;

  assert(RestoreBatchCompletionIndex(0, 1) == 0);
  assert(RestoreBatchCompletionIndex(0, 4) == 3);
  assert(RestoreBatchCompletionIndex(1, 4) == 2);
  assert(RestoreBatchCompletionIndex(2, 4) == 1);
  assert(RestoreBatchCompletionIndex(3, 4) == 0);

  std::vector<std::size_t> preparation_order;
  const std::size_t observed =
      cuda_checkpoint_operation::RunRestoreBatchPreparation(
          4, [&](std::size_t index) { preparation_order.push_back(index); });
  assert((preparation_order == std::vector<std::size_t>{0, 1, 2, 3}));
  assert(observed == 1);

  bool exception_observed = false;
  std::size_t first_exception = 4;
  std::vector<std::size_t> attempted;
  try {
    cuda_checkpoint_operation::RunRestoreBatchPreparation(
        4,
        [&](std::size_t index) {
          attempted.push_back(index);
          if (index == 2)
            throw std::runtime_error("prepare failure");
        },
        &first_exception);
  } catch (const std::runtime_error &error) {
    exception_observed = std::string(error.what()) == "prepare failure";
  }
  assert(exception_observed);
  assert(first_exception == 2);
  assert((attempted == std::vector<std::size_t>{0, 1, 2}));

  bool limit_observed = false;
  try {
    cuda_checkpoint_operation::RunRestoreBatchPreparation(
        kMaxRestoreBatchTargets + 1, [](std::size_t) {});
  } catch (const std::invalid_argument &) {
    limit_observed = true;
  }
  assert(limit_observed);

  const std::map<std::string, RestoreBatchJobFileIdentity> identities{
      {"/jobs/a", {.present = true, .device = 1, .inode = 10}},
      {"/aliases/a", {.present = true, .device = 1, .inode = 10}},
      {"/jobs/b", {.present = true, .device = 1, .inode = 11}},
  };
  std::vector<RestoreBatchJobFileGroup> groups;
  std::string grouping_error;
  assert(GroupRestoreBatchJobFiles(
      {"/jobs/a", "/jobs/b", "/aliases/a", ""},
      [&](const std::string &path, RestoreBatchJobFileIdentity *identity,
          std::string *error) {
        const auto found = identities.find(path);
        if (found == identities.end()) {
          *error = "unknown path";
          return false;
        }
        *identity = found->second;
        return true;
      },
      &groups, &grouping_error));
  assert(groups.size() == 3);
  assert(groups[0].representative == "/jobs/a");
  assert((groups[0].request_indices == std::vector<std::size_t>{0, 2}));
  assert(groups[1].representative == "/jobs/b");
  assert((groups[1].request_indices == std::vector<std::size_t>{1}));
  assert(groups[2].representative.empty());
  assert((groups[2].request_indices == std::vector<std::size_t>{3}));

  // Aliases of the exact same launch-job inode are prepared serially in
  // captured process-tree order.
  std::vector<std::size_t> alias_order;
  const std::size_t alias_width =
      cuda_checkpoint_operation::RunRestoreBatchPreparation(
          groups[0].request_indices.size(),
          [&](std::size_t group_index) {
            alias_order.push_back(groups[0].request_indices[group_index]);
          });
  assert(alias_width == 1);
  assert((alias_order == std::vector<std::size_t>{0, 2}));

  // Cancellation remains cooperative and is visible to each later participant.
  bool cancelled = false;
  std::size_t cancellation_observers = 0;
  cuda_checkpoint_operation::RunRestoreBatchPreparation(
      4, [&](std::size_t index) {
        if (index == 0)
          cancelled = true;
        if (cancelled)
          ++cancellation_observers;
      });
  assert(cancellation_observers == 4);
  return 0;
}

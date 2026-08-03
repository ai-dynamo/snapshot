// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
#include "pagebroker.hpp"
#include <cassert>
#include <cstdio>
#include <fstream>
#include <unistd.h>
int main() {
  auto root = std::filesystem::temp_directory_path() /
              ("pagebroker-test-" + std::to_string(getpid()));
  std::filesystem::remove_all(root);
  auto source = root / "source";
  std::filesystem::create_directories(source);
  std::ofstream(source / "image").write("checkpoint", 10);
  pagebroker::TransactionManager manager(root / "staging", root / "scratch",
                                         10);
  pagebroker::Request submit{pagebroker::Request::Operation::Submit, "tx-1",
                             source};
  auto ok = manager.submit(submit);
  assert(ok.ok);
  assert(std::filesystem::exists(root / "staging/tx/tx-1/image"));
  pagebroker::TransactionManager concurrent_manager(root / "staging-concurrent",
                                                    root / "scratch-concurrent",
                                                    15);
  auto first = concurrent_manager.submit(submit);
  assert(first.ok);
  auto second = concurrent_manager.submit(
      pagebroker::Request{pagebroker::Request::Operation::Submit, "tx-2",
                          source});
  assert(!second.ok);
  assert(concurrent_manager.abort(submit).ok);
  auto duplicate = manager.submit(submit);
  assert(!duplicate.ok);
  auto second_rejected = manager.submit(
      pagebroker::Request{pagebroker::Request::Operation::Submit, "tx-2",
                          source});
  assert(!second_rejected.ok);
  auto committed = manager.commit(submit);
  assert(committed.ok);
  assert(!std::filesystem::exists(root / "staging/tx/tx-1"));
  auto too_big =
      pagebroker::TransactionManager(root / "staging2", root / "scratch2", 1)
          .submit(submit);
  assert(!too_big.ok);

  auto destination = root / "checkpoints" / "nested" / "final";
  pagebroker::TransactionManager checkpoint_manager(root / "staging3",
                                                     root / "scratch3", 100);
  pagebroker::Request prepare{pagebroker::Request::Operation::PrepareCheckpoint,
                              "tx-2", destination};
  auto prepared = checkpoint_manager.prepare_checkpoint(prepare);
  assert(prepared.ok);
  std::ofstream(std::filesystem::path(prepared.staging_path) / "manifest")
      .write("ok", 2);
  assert(checkpoint_manager.commit(prepare).ok);
  assert(std::filesystem::exists(destination / "manifest"));

  auto leaked = checkpoint_manager.prepare_checkpoint(
      pagebroker::Request{pagebroker::Request::Operation::PrepareCheckpoint,
                          "tx-3", root / "checkpoints" / "leaked"});
  if (!leaked.ok)
    std::fprintf(stderr, "prepare leaked failed: %s\n", leaked.error.c_str());
  assert(leaked.ok);
  pagebroker::TransactionManager restarted(root / "staging3", root / "scratch3",
                                           100);
  assert(!std::filesystem::exists(
      std::filesystem::path(leaked.staging_path)));

  auto budget_destination = root / "checkpoints" / "budget";
  pagebroker::TransactionManager budget_manager(root / "staging4",
                                                root / "scratch4", 1);
  auto budget_tx = budget_manager.prepare_checkpoint(
      pagebroker::Request{pagebroker::Request::Operation::PrepareCheckpoint,
                          "tx-4", budget_destination});
  assert(budget_tx.ok);
  std::ofstream(std::filesystem::path(budget_tx.staging_path) / "large")
      .write("12", 2);
  assert(!budget_manager.commit(
                   pagebroker::Request{
                       pagebroker::Request::Operation::Commit, "tx-4", {}})
              .ok);
  assert(budget_manager.abort(
                 pagebroker::Request{
                       pagebroker::Request::Operation::Abort, "tx-4", {}})
             .ok);
  std::filesystem::remove_all(root);
}

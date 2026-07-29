// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
#include "pagebroker.hpp"
#include <cassert>
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
                                         100);
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
  auto committed = manager.commit(submit);
  assert(committed.ok);
  assert(!std::filesystem::exists(root / "staging/tx/tx-1"));
  auto too_big =
      pagebroker::TransactionManager(root / "staging2", root / "scratch2", 1)
          .submit(submit);
  assert(!too_big.ok);

  std::filesystem::remove_all(root);
}

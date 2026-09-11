/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#pragma once

#include <atomic>
#include <chrono>
#include <cstdio>
#include <thread>

namespace cuda_checkpoint_transfer {

// Ambiguous backend faults must preserve the failing worker's entire stack:
// queued I/O may still reference its buffers. Only the service can authorize
// process teardown, after all current and previously restored targets exit.
class FatalIo {
public:
  static bool Pending() { return pending_.load(std::memory_order_acquire); }

  [[noreturn]] static void Contain() {
    pending_.store(true, std::memory_order_release);
    for (;;) {
      std::this_thread::sleep_for(std::chrono::seconds(1));
    }
  }

  // POSIX queue errors can leave callbacks referencing submitted handles;
  // neither post nor poll failure establishes that buffers are safe to free.
  static void CheckBackendStatus(const char *operation, int status,
                                 int success, int in_progress) {
    if (status == success || status == in_progress) {
      return;
    }
    std::fprintf(stderr,
                 "fatal: NIXL restore %s failed with status %d; "
                 "retaining I/O resources until targets exit\n",
                 operation, status);
    std::fflush(stderr);
    Contain();
  }

  template <class Current, class Retained, class Retry, class Terminate>
  static void QuiesceBeforeTeardown(Current current, Retained retained,
                                   Retry retry, Terminate terminate) {
    for (;;) {
      // Neither an inconclusive result nor an exception may skip the other
      // group: previous restores can still depend on this helper's contexts.
      const bool current_exited = TryQuiesce(current);
      const bool retained_exited = TryQuiesce(retained);
      if (current_exited && retained_exited) {
        break;
      }
      retry();
    }
    terminate();
  }

private:
  template <class Quiesce> static bool TryQuiesce(Quiesce &quiesce) {
    try {
      return quiesce();
    } catch (...) {
      // Missing identity/termination evidence is not permission to unwind I/O.
      return false;
    }
  }

  inline static std::atomic<bool> pending_{false};
};

} // namespace cuda_checkpoint_transfer

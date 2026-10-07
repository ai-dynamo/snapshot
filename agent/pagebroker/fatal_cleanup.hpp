// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0
#pragma once

#include <atomic>
#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <thread>

namespace snapshot::pagebroker {
namespace detail {
inline std::atomic<bool> fatal_cleanup{false};
}

inline bool FatalCleanupPending()
{
  return detail::fatal_cleanup.load(std::memory_order_acquire);
}

// Start the watchdog before attempting cleanup that may itself block.
inline void SignalFatalCleanup()
{
  detail::fatal_cleanup.store(true, std::memory_order_release);
}

// Keep this thread's entire ownership stack alive. In particular, a failed
// unmap must not unwind into allocation release, and pending I/O still owns
// its buffers and file. The daemon's shutdown owner terminates the process.
[[noreturn]] inline void ReportFatalCleanup(const char* message)
{
  // Signal before logging so a blocked stderr cannot prevent shutdown.
  SignalFatalCleanup();
  std::fprintf(stderr, "PageBroker fatal cleanup: %s\n", message);
  std::fflush(stderr);
  for (;;) {
    detail::fatal_cleanup.wait(true);
  }
}

// The independent watchdog also bounds failures on the main thread or during
// destruction. Other operations get a cancellation window before process exit.
class FatalCleanupShutdown {
 public:
  explicit FatalCleanupShutdown(std::atomic<bool>& stopping,
      std::chrono::milliseconds timeout = std::chrono::seconds{30})
      : worker_([&stopping, timeout](std::stop_token stop) {
          while (!FatalCleanupPending()) {
            if (stop.stop_requested()) {
              return;
            }
            std::this_thread::sleep_for(std::chrono::milliseconds{10});
          }
          stopping.store(true, std::memory_order_relaxed);
          std::this_thread::sleep_for(timeout);
          std::_Exit(EXIT_FAILURE);
        }) {}

 private:
  std::jthread worker_;
};
}  // namespace snapshot::pagebroker

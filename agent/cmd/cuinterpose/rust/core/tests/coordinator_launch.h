/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef CUINTERPOSE_RUST_TEST_COORDINATOR_LAUNCH_H
#define CUINTERPOSE_RUST_TEST_COORDINATOR_LAUNCH_H

#include <unistd.h>

#include <algorithm>
#include <cerrno>
#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <thread>

namespace cuinterpose_test {

struct ForkResult {
  pid_t pid;
  int error;
  unsigned retries;
};

// Only coordinator process creation is retried, never an executed command.
// Injection keeps deadline/error coverage deterministic without changing fork
// process-wide or altering any of the actual fork-generation test cases.
template <typename Fork, typename Now, typename Pause>
ForkResult bounded_coordinator_fork(Fork fork_process, Now now, Pause pause,
                                    std::chrono::steady_clock::duration budget) {
  const auto deadline = now() + budget;
  unsigned retries = 0;
  for (;;) {
    const pid_t pid = fork_process();
    if (pid >= 0) return {pid, 0, retries};
    const int error = errno;
    if (error != EAGAIN) return {-1, error, retries};
    const auto current = now();
    if (current >= deadline) return {-1, EAGAIN, retries};
    pause(std::min(deadline - current,
                   std::chrono::steady_clock::duration(std::chrono::milliseconds(1))));
    if (now() >= deadline) return {-1, EAGAIN, retries};
    ++retries;
  }
}

inline pid_t fork_coordinator() {
  const auto result = bounded_coordinator_fork(
      ::fork, std::chrono::steady_clock::now,
      [](std::chrono::steady_clock::duration delay) { std::this_thread::sleep_for(delay); },
      std::chrono::seconds(5));
  // Return immediately in the child: it must neither log nor retry creation.
  if (result.pid == 0) return 0;
  if (result.retries != 0 || result.pid < 0) {
    std::fprintf(stderr, "coordinator launch adapter v1: EAGAIN retries=%u, errno=%d\n",
                 result.retries, result.error);
  }
  if (result.pid < 0) {
    // Never allow the original helper to call waitpid(-1) and accidentally
    // reap a workload participant after coordinator creation failed.
    std::fprintf(stderr, "coordinator creation failed; no command was launched\n");
    std::abort();
  }
  return result.pid;
}

}  // namespace cuinterpose_test
#endif

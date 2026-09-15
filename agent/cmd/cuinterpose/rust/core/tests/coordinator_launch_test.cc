/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * SPDX-License-Identifier: Apache-2.0
 */

#include "coordinator_launch.h"

#include <cassert>

int main() {
  using Clock = std::chrono::steady_clock;
  using namespace std::chrono_literals;
  Clock::time_point clock{};
  unsigned calls = 0;
  unsigned successes = 0;
  auto now = [&] { return clock; };
  auto pause = [&](Clock::duration delay) { clock += delay; };

  const auto recovered = cuinterpose_test::bounded_coordinator_fork(
      [&]() -> pid_t {
        if (++calls <= 3) {
          errno = EAGAIN;
          return -1;
        }
        ++successes;
        return 123;
      }, now, pause, 5ms);
  assert(recovered.pid == 123 && recovered.error == 0 && recovered.retries == 3);
  assert(calls == 4 && successes == 1 && clock == Clock::time_point{} + 3ms);

  calls = 0;
  clock = {};
  const auto terminal = cuinterpose_test::bounded_coordinator_fork(
      [&]() -> pid_t { ++calls; errno = ENOMEM; return -1; }, now, pause, 5ms);
  assert(terminal.pid == -1 && terminal.error == ENOMEM && terminal.retries == 0);
  assert(calls == 1 && clock == Clock::time_point{});

  calls = 0;
  clock = {};
  const auto exhausted = cuinterpose_test::bounded_coordinator_fork(
      [&]() -> pid_t { ++calls; errno = EAGAIN; return -1; }, now, pause, 5ms);
  assert(exhausted.pid == -1 && exhausted.error == EAGAIN && exhausted.retries == 4);
  assert(calls == 5 && clock == Clock::time_point{} + 5ms);

  calls = 0;
  clock = {};
  const auto delayed = cuinterpose_test::bounded_coordinator_fork(
      [&]() -> pid_t { ++calls; clock += 6ms; errno = EAGAIN; return -1; },
      now, pause, 5ms);
  assert(delayed.pid == -1 && delayed.error == EAGAIN && delayed.retries == 0);
  assert(calls == 1 && clock == Clock::time_point{} + 6ms);

  calls = 0;
  clock = {};
  const auto child = cuinterpose_test::bounded_coordinator_fork(
      [&]() -> pid_t { ++calls; return 0; }, now, pause, 5ms);
  assert(child.pid == 0 && child.error == 0 && child.retries == 0);
  assert(calls == 1 && clock == Clock::time_point{});

  std::puts("PASS 5 coordinator-launch adapter cases; no CUDA/phase retries");
}

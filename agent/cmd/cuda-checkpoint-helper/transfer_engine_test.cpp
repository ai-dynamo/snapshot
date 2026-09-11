/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#include "transfer_cancellation.h"
#include "restore_pipeline.h"

#include <algorithm>
#include <cassert>
#include <chrono>
#include <limits>
#include <vector>
#include <sys/wait.h>
#include <unistd.h>

namespace {

using cuda_checkpoint_transfer::RestoreProgress;
using cuda_checkpoint_transfer::RunRestorePipeline;

struct FakeRestore {
  struct Slot {
    size_t chunk = 0;
    int ready = 0;
    bool reading = false;
    bool copying = false;
  };
  std::vector<Slot> slots = std::vector<Slot>(4);
  std::vector<size_t> started;
  std::vector<size_t> copied;
  size_t fail_submit = SIZE_MAX;
  size_t fail_read = SIZE_MAX;
  size_t fail_copy = SIZE_MAX;
  size_t fail_copy_completion = SIZE_MAX;
  int cancel_at = std::numeric_limits<int>::max();
  int tick = 0;
  size_t active_reads = 0;
  size_t peak_reads = 0;

  bool Cancelled() const { return tick >= cancel_at; }
  bool Read(size_t chunk, size_t index) {
    Slot &slot = slots[index];
    assert(!slot.reading && !slot.copying);
    slot = {chunk, tick + (chunk == 0 ? 8 : 1), true, false};
    started.push_back(chunk);
    peak_reads = std::max(peak_reads, ++active_reads);
    return chunk != fail_submit;
  }
  RestoreProgress PollRead(size_t index) {
    Slot &slot = slots[index];
    assert(slot.reading);
    if (tick < slot.ready) {
      return RestoreProgress::kPending;
    }
    slot.reading = false;
    --active_reads;
    return slot.chunk == fail_read ? RestoreProgress::kFailed
                                   : RestoreProgress::kComplete;
  }
  bool Copy(size_t chunk, size_t index) {
    Slot &slot = slots[index];
    assert(!slot.reading && !slot.copying);
    assert(slot.chunk == chunk);
    if (chunk == fail_copy) {
      return false;
    }
    slot.copying = true;
    slot.ready = tick + 2;
    copied.push_back(chunk);
    return true;
  }
  RestoreProgress PollCopy(size_t index) {
    Slot &slot = slots[index];
    assert(slot.copying);
    if (tick < slot.ready) {
      return RestoreProgress::kPending;
    }
    slot.copying = false;
    return slot.chunk == fail_copy_completion ? RestoreProgress::kFailed
                                              : RestoreProgress::kComplete;
  }
  void Idle(bool) {
    ++tick;
    assert(tick < 100);
  }
  void AssertDrained() const {
    assert(active_reads == 0);
    for (const auto &slot : slots) {
      assert(!slot.reading && !slot.copying);
    }
  }
};

void TestRestoreFillsAndReplenishesWithoutWaitingForFirstRead() {
  FakeRestore io;
  assert(RunRestorePipeline(12, io.slots.size(), io));
  assert(io.peak_reads == io.slots.size());
  assert(io.started.size() == 12 && io.copied.size() == 12);
  assert(std::find(io.copied.begin(), io.copied.end(), 4) <
         std::find(io.copied.begin(), io.copied.end(), 0));
  std::sort(io.copied.begin(), io.copied.end());
  assert(io.copied == io.started);
  io.AssertDrained();
}

void TestRestoreFailureStopsReplenishmentAndDrainsOwners() {
  for (int failure = 0; failure < 6; ++failure) {
    FakeRestore io;
    if (failure == 0) {
      io.fail_submit = 1;
    } else if (failure == 1) {
      io.fail_read = 1;
    } else if (failure == 2) {
      io.fail_copy = 2;
    } else if (failure == 3) {
      io.cancel_at = 1;
    } else if (failure == 4) {
      io.cancel_at = 2;
    } else {
      io.fail_copy_completion = 1;
    }
    assert(!RunRestorePipeline(12, io.slots.size(), io));
    assert(io.started.size() <= io.slots.size());
    io.AssertDrained();
  }
}

void TestRestoreHandlesEmptyAndSingleSlot() {
  FakeRestore empty;
  assert(RunRestorePipeline(0, 4, empty));
  assert(empty.started.empty());
  FakeRestore one;
  assert(RunRestorePipeline(3, 1, one));
  assert(one.peak_reads == 1);
  assert(one.started == one.copied);
  one.AssertDrained();
}

} // namespace

// Inject the same post/poll boundary and two-group termination policy used by
// the native adapter and service, without requiring CUDA or a real I/O fault.
static void TestBackendFaultRetainsIoUntilBothTargetGroupsExit(
    const char *operation, bool retained_indeterminate, bool throws) {
  using cuda_checkpoint_transfer::FatalIo;
  const pid_t child = fork();
  assert(child >= 0);
  if (child == 0) {
    alarm(5);
    std::atomic<bool> buffer_alive{false};
    std::thread worker([&] {
      struct PendingBuffer {
        ~PendingBuffer() { _exit(91); }
      } pending_buffer;
      buffer_alive.store(true);
      // Model a post error after partial submission, or a poll error while
      // callbacks still own the buffer. Neither path may unwind this frame.
      FatalIo::CheckBackendStatus(operation, -1, 0, 1);
      _exit(92);
    });
    while (!FatalIo::Pending()) {
      std::this_thread::yield();
    }
    assert(cuda_checkpoint_transfer::TransferCancellation{}.IsCancelled());
    int current_attempts = 0;
    int retained_attempts = 0;
    int retries = 0;
    auto terminate_group = [&](bool indeterminate, int &attempts) {
      assert(buffer_alive.load());
      if (++attempts < 3 && indeterminate) {
        if (throws) {
          throw 1;
        }
        return false;
      }
      return true;
    };
    FatalIo::QuiesceBeforeTeardown(
        [&] {
          return terminate_group(!retained_indeterminate, current_attempts);
        },
        [&] {
          return terminate_group(retained_indeterminate, retained_attempts);
        },
        [&] {
          assert(buffer_alive.load());
          assert(current_attempts == retained_attempts);
          assert(current_attempts < 3);
          ++retries;
        },
        [&] {
          assert(current_attempts == 3 && retained_attempts == 3);
          assert(buffer_alive.load() && retries == 2);
          _exit(0);
        });
    _exit(92);
  }
  int status = 0;
  assert(waitpid(child, &status, 0) == child);
  assert(WIFEXITED(status) && WEXITSTATUS(status) == 0);
}

int main() {
  using cuda_checkpoint_transfer::FatalIo;
  FatalIo::CheckBackendStatus("post", 0, 0, 1);
  FatalIo::CheckBackendStatus("poll", 1, 0, 1);
  assert(!FatalIo::Pending());
  for (const char *operation : {"post", "poll"}) {
    for (bool retained_indeterminate : {false, true}) {
      for (bool throws : {false, true}) {
        TestBackendFaultRetainsIoUntilBothTargetGroupsExit(
            operation, retained_indeterminate, throws);
      }
    }
  }
  TestRestoreFillsAndReplenishesWithoutWaitingForFirstRead();
  TestRestoreFailureStopsReplenishmentAndDrainsOwners();
  TestRestoreHandlesEmptyAndSingleSlot();
  using namespace std::chrono_literals;
  using cuda_checkpoint_transfer::TransferCancellation;

  TransferCancellation unlimited;
  assert(!unlimited.DeadlineExceeded());
  assert(!unlimited.IsCancelled());

  TransferCancellation active(TransferCancellation::Clock::now() + 1h);
  assert(!active.DeadlineExceeded());
  assert(!active.IsCancelled());

  TransferCancellation expired(TransferCancellation::Clock::now() - 1ms);
  assert(expired.DeadlineExceeded());
  assert(expired.IsCancelled());

  active.Cancel();
  assert(active.IsCancelled());
  return 0;
}

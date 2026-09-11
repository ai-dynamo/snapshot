/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#pragma once

#include <cstddef>
#include <vector>

namespace cuda_checkpoint_transfer {

enum class RestoreProgress { kPending, kComplete, kFailed };

// A slot belongs to storage until PollRead completes, then to CUDA until
// PollCopy completes. On failure, drain both owners without posting more work.
// The adapter owns request handles, buffers, diagnostics, and polling delays.
template <typename Adapter>
bool RunRestorePipeline(size_t chunk_count, size_t slot_count, Adapter &adapter) {
  if (slot_count == 0) {
    return false;
  }
  enum class State { kIdle, kReading, kCopying };
  struct Slot {
    State state = State::kIdle;
    size_t chunk = 0;
  };
  std::vector<Slot> slots(slot_count);
  size_t next = 0;
  bool success = true;
  for (;;) {
    bool progressed = false;
    bool active = false;
    for (size_t index = 0; index < slots.size(); ++index) {
      Slot &slot = slots[index];
      if (success && adapter.Cancelled()) {
        success = false;
      }
      if (slot.state == State::kReading) {
        const RestoreProgress result = adapter.PollRead(index);
        if (result != RestoreProgress::kPending) {
          progressed = true;
          slot.state = State::kIdle;
          if (result == RestoreProgress::kFailed) {
            success = false;
          } else if (success) {
            if (adapter.Copy(slot.chunk, index)) {
              slot.state = State::kCopying;
            } else {
              success = false;
            }
          }
        }
      }
      if (slot.state == State::kCopying) {
        const RestoreProgress result = adapter.PollCopy(index);
        if (result != RestoreProgress::kPending) {
          progressed = true;
          slot.state = State::kIdle;
          if (result == RestoreProgress::kFailed) {
            success = false;
          }
        }
      }
      if (success && slot.state == State::kIdle && next < chunk_count) {
        slot.chunk = next++;
        // Even a failed submission may own a request that needs draining.
        slot.state = State::kReading;
        if (!adapter.Read(slot.chunk, index)) {
          success = false;
        }
        progressed = true;
      }
      active = active || slot.state != State::kIdle;
    }
    if (!active && (!success || next == chunk_count)) {
      return success;
    }
    adapter.Idle(progressed);
  }
}

} // namespace cuda_checkpoint_transfer

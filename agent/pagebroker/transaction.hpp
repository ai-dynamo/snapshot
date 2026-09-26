// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <chrono>
#include <condition_variable>
#include <mutex>
#include <variant>
#include <map>
#include <set>
#include <cstdint>
#include <sys/types.h>

#include "checkpoint_transaction_descriptor.hpp"
#include "restore_transaction_descriptor.hpp"

namespace snapshot::pagebroker {
// A borrowed immutable artifact, never owned by transaction cleanup. Consumers
// open relative to this descriptor rather than resolving the source path again.
struct DirectRestoreDescriptor {
  FileDescriptor source_directory;
};

class Transaction {
 public:
  enum class State { NEW, PREPARING, STAGED, COMMITTED, ABORTED };
  using Descriptor = std::variant<std::monostate, RestoreTransactionDescriptor, CheckpointTransactionDescriptor,
                                  DirectRestoreDescriptor>;

  // Callers hold mutex() while accessing transaction state.
  std::mutex& mutex();
  State state() const;
  void set_state(State state);
  const Descriptor& descriptor() const;
  void set_descriptor(Descriptor descriptor);
  void clear_descriptor();
  bool retain_terminal();
  bool expired(std::chrono::steady_clock::time_point now, std::chrono::steady_clock::duration lifetime) const;

  // Protected by mutex(). Sessions own admission until the engine acknowledges drain.
  // A failed/unfinished session makes publication invalid, but permits Abort
  // after the last session has drained. Target IDs cannot be rebound.
  size_t native_sessions = 0;
  std::condition_variable native_drained;
  bool native_failed = false;
  std::set<uint32_t> native_targets;
  // Host PIDs by PID as seen from a pinned native PID namespace (keyed by the
  // namespace's st_dev/st_ino), recorded by one /proc scan after CRIU. Entries
  // are re-verified before use; -1 marks a PID claimed by more than one process.
  std::map<std::pair<dev_t, ino_t>, std::map<uint32_t, int>> native_host_pids;

 private:
  std::mutex mutex_;
  State state_ = State::NEW;
  Descriptor descriptor_;
  std::chrono::steady_clock::time_point staging_started_at_;
  bool terminal_retained_ = false;
};
}  // namespace snapshot::pagebroker

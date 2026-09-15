// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <atomic>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <exception>
#include <future>
#include <memory>
#include <mutex>
#include <string>
#include <unordered_map>
#include <vector>

#include "restore_plan.hpp"
#include "utils/event_loop.hpp"

namespace snapshot::pagebroker {
// Materializes restore plans through Model Streamer while one event loop
// coordinates multiple in-flight submissions and their responses.
class ModelStreamerRestore {
 public:
  // Creates an inactive restore coordinator that starts on its first Stage call.
  explicit ModelStreamerRestore(std::chrono::milliseconds submission_timeout = std::chrono::hours(2));
  // Stops the event loop and streamer, then fails any unfinished submissions.
  ~ModelStreamerRestore() noexcept;
  // Prevents copying ownership of the event loop and native streamer handle.
  ModelStreamerRestore(const ModelStreamerRestore&) = delete;
  // Prevents replacing an instance that owns active restore state.
  ModelStreamerRestore& operator=(const ModelStreamerRestore&) = delete;
  // Reports a terminal event-loop failure after native access has stopped and
  // before any failed submission waiter is released.
  bool Failed() const noexcept;
  // Creates the destination tree and blocks until all planned data is restored.
  void Stage(const RestorePlan& plan, const Path& destination);

 private:
  // Owns one submission's ABI arrays, response progress, and completion signal.
  // Its pointer targets remain alive in the calling Stage operation.
  struct StreamerEntry {
    // Collects the parallel argument arrays passed to the Model Streamer API.
    struct Request {
      std::vector<const char*> paths;
      std::vector<unsigned> range_counts;
      std::vector<std::size_t> offsets;
      std::vector<std::size_t> sizes;
      std::vector<void*> destinations;
    } request;

    std::vector<bool> completed;
    std::size_t responses_received = 0;
    std::string first_error;
    std::promise<void> completion;
    std::chrono::steady_clock::time_point deadline;
  };

  // Holds one response returned by the Model Streamer API for dispatch by ID.
  struct StreamerResponse {
    int status = 0;
    std::uint64_t submission_id = 0;
    unsigned file_index = 0;
    unsigned range_index = 0;
    int submission_done = 0;
  };

  // Runs one native submission on the event-loop thread and transfers accepted
  // entries into the active-submission map.
  class SubmitEvent final : public utils::Event {
   public:
    // Takes ownership of an entry until the streamer accepts or rejects it.
    SubmitEvent(ModelStreamerRestore& restore, std::unique_ptr<StreamerEntry> entry);
    // Submits the entry to Model Streamer from the event-loop thread.
    void Execute() override;
    // Fails an entry that was still queued when the event loop stopped.
    void Cancel(std::exception_ptr error) noexcept override;

   private:
    ModelStreamerRestore& restore_;
    std::unique_ptr<StreamerEntry> entry_;
  };

  // Polls for one native response and keeps polling while submissions are active.
  class ReceiveEvent final : public utils::Event {
   public:
    // Associates the receive operation with its restore coordinator.
    explicit ReceiveEvent(ModelStreamerRestore& restore);
    // Receives and dispatches one response, then schedules the next poll.
    void Execute() override;
    // Does nothing because event-loop failure handling fails all active entries.
    void Cancel(std::exception_ptr error) noexcept override;

   private:
    ModelStreamerRestore& restore_;
  };

  // Starts the native Model Streamer session and its event-loop worker.
  void Start();
  // Restores files in byte-bounded submissions and then applies their permissions.
  void RestoreFiles(const RestorePlan& plan, const Path& destination);
  // Sends an entry to Model Streamer and registers its assigned submission ID.
  void Submit(std::unique_ptr<StreamerEntry>& entry);
  // Applies one response to an entry and reports whether the submission finished.
  bool AcceptEntry(StreamerEntry& entry, const StreamerResponse& response);
  // Completes one entry with an exception without allowing failure to escape.
  void FailEntry(StreamerEntry& entry, std::exception_ptr error) noexcept;
  // Polls for one response and dispatches it to the matching active entry.
  void ReceiveAndDispatch();
  // Queues a receive poll when active work has no poll already scheduled.
  void ScheduleReceive();
  // Ends the native Model Streamer session if it is running.
  void StopStreamer() noexcept;
  // Stops native access and fails active work after an event-loop failure.
  void HandleFailure(std::exception_ptr error) noexcept;
  // Completes every active entry with the supplied exception and clears state.
  void FailAll(std::exception_ptr error) noexcept;

  std::once_flag start_once_;
  void* value_ = nullptr;
  const std::chrono::milliseconds submission_timeout_;
  const std::exception_ptr stopped_error_;
  utils::EventLoop event_loop_;
  std::unordered_map<std::uint64_t, std::unique_ptr<StreamerEntry>> active_;
  bool receive_scheduled_ = false;
  std::atomic<bool> failed_ = false;
};
}  // namespace snapshot::pagebroker

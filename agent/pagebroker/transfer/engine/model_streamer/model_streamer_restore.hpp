// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <atomic>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <deque>
#include <exception>
#include <future>
#include <memory>
#include <mutex>
#include <string>
#include <unordered_map>
#include <vector>

#include "transfer/checkpoint_inventory.hpp"
#include "cancellation.hpp"
#include "utils/event_loop.hpp"

namespace snapshot::pagebroker {
// Explicit values are fixed for one native session. Empty credentials select
// the native AWS provider chain. Addressing, TLS/CA and worker configuration use
// the pinned library's process environment, set before constructing sessions.
struct ModelStreamerSessionOptions {
  std::string region;
  std::string endpoint;
  std::string access_key_id;
  std::string secret_access_key;
  std::string session_token;
};

// Materializes restore inventories through Model Streamer while one event loop
// coordinates multiple in-flight submissions and their responses. Concurrent
// Stage calls must use separate destinations. Only one coordinator may run a
// native session per process; independent concurrent coordinators are unsupported.
// A failed coordinator may be replaced after its native session has stopped.
class ModelStreamerRestore {
 public:
  // Creates an inactive restore coordinator that starts on its first Stage call.
  explicit ModelStreamerRestore(std::chrono::milliseconds submission_timeout = std::chrono::hours(2));
  explicit ModelStreamerRestore(
      ModelStreamerSessionOptions options,
      std::chrono::milliseconds submission_timeout = std::chrono::hours(2));
  // Stops the event loop and native session. Admitted submissions retain their
  // buffers until native access ends.
  ~ModelStreamerRestore() noexcept;
  // Prevents copying ownership of the event loop and native streamer handle.
  ModelStreamerRestore(const ModelStreamerRestore&) = delete;
  // Prevents replacing an instance that owns active restore state.
  ModelStreamerRestore& operator=(const ModelStreamerRestore&) = delete;
  // Reports a terminal event-loop failure once native access has stopped.
  bool Failed() const noexcept;
  // Creates the destination tree and blocks until all planned data is restored.
  void Stage(const CheckpointInventory& inventory, const Path& destination, Cancellation cancellation = {});

 private:
  enum class Admission { Pending, Claimed, Cancelled };

  // Owns one submission's mappings, ABI arrays, response progress, and completion
  // signal. Paths borrow the calling Stage operation's inventory; only a successful
  // admission claim permits native access before that caller returns.
  struct StreamerEntry {
    StreamerEntry() = default;
    ~StreamerEntry() noexcept;
    StreamerEntry(const StreamerEntry&) = delete;
    StreamerEntry& operator=(const StreamerEntry&) = delete;
    // Called by the restore caller before admission or after native access ends.
    // Repeated calls are harmless; destruction also releases remaining mappings.
    void ReleaseMappings() noexcept;
    // Adds a nonempty file, or returns false when it belongs in the next batch.
    // On exception, discard the entry; its arrays may be partially populated.
    bool TryAddFile(const CheckpointFile& file, const Path& destination);

    // Collects the parallel argument arrays passed to the Model Streamer API.
    struct Request {
      std::vector<const char*> paths;
      std::vector<unsigned> range_counts;
      std::vector<std::size_t> offsets;
      std::vector<std::size_t> sizes;
      std::vector<void*> destinations;
    } request;

    uintmax_t bytes = 0;
    std::vector<bool> completed;
    std::size_t responses_received = 0;
    std::string first_error;
    std::promise<void> completion;
    // The caller and worker race to cancel or claim this entry before submission.
    std::atomic<Admission> admission{Admission::Pending};
    std::chrono::steady_clock::time_point deadline;
    Cancellation cancellation;
  };

  // Holds one response returned by the Model Streamer API for dispatch by ID.
  struct StreamerResponse {
    int status = 0;
    std::uint64_t submission_id = 0;
    unsigned file_index = 0;
    unsigned range_index = 0;
    int submission_done = 0;
  };

  // Queues a submission on the event-loop thread and schedules processing.
  class SubmitEvent final : public utils::Event {
   public:
    // Shares ownership with the caller until Execute transfers the entry to the
    // pending queue.
    SubmitEvent(ModelStreamerRestore& restore, std::shared_ptr<StreamerEntry> entry);
    // Adds the entry to the pending queue from the event-loop thread.
    void Execute() override;
    // Fails an entry that was still queued when the event loop stopped.
    void Cancel(std::exception_ptr error) noexcept override;

   private:
    ModelStreamerRestore& restore_;
    std::shared_ptr<StreamerEntry> entry_;
  };

  // Advances admission, native responses, and session completion on the event loop.
  class WorkEvent final : public utils::Event {
   public:
    // Associates the work event with its restore coordinator.
    explicit WorkEvent(ModelStreamerRestore& restore);
    // Processes one turn and schedules another while work remains.
    void Execute() override;
    // Does nothing; failure handling or shutdown fails all retained entries.
    void Cancel(std::exception_ptr error) noexcept override;

   private:
    ModelStreamerRestore& restore_;
  };

  // Starts a native session on the event-loop thread.
  void Start();
  // Restores files in batches using a byte budget; an oversized file forms its
  // own batch. Applies file permissions after restoration.
  void RestoreFiles(const CheckpointInventory& inventory, const Path& destination, Cancellation cancellation);
  // Submits an entry and waits, allowing cancellation before admission.
  // Claimed buffers remain alive until native access ends.
  void SubmitAndWait(const std::shared_ptr<StreamerEntry>& entry);
  // Queues an entry and ensures a work event is scheduled.
  void QueueSubmission(std::shared_ptr<StreamerEntry>& entry);
  // Admits a bounded group of queued submissions into one native session.
  void SubmitPending();
  // Queued entries have no native readers and can be cancelled independently.
  void DiscardInterruptedPending();
  bool CanAdmit(const StreamerEntry& entry) const;
  // Claims an entry, submits it to Model Streamer, and registers its assigned ID.
  void AdmitAndSubmit(std::shared_ptr<StreamerEntry>& entry);
  // Returns the native ID, or zero after failing a rejected entry.
  // Throws when the error requires ending the native session.
  std::uint64_t SubmitToStreamer(StreamerEntry& entry);
  // Ends a drained session before releasing failed submissions' destinations.
  void FinishSession();
  // Applies one response to an entry and reports whether the submission finished.
  bool AcceptEntry(StreamerEntry& entry, const StreamerResponse& response);
  // Completes a failed entry using its own cancellation reason, if interrupted.
  void FailEntry(StreamerEntry& entry, std::exception_ptr error) noexcept;
  // Admits pending entries, polls one response, and finishes a drained session.
  void ProcessWork();
  // Queues one work event if pending or active entries need processing.
  void ScheduleWork();
  // Ends the native Model Streamer session if it is running.
  void StopStreamer() noexcept;
  // Stops native access and fails active work after an event-loop failure.
  void HandleFailure(std::exception_ptr error) noexcept;
  // Fails active and pending entries through FailEntry, then clears state.
  void FailAll(std::exception_ptr error) noexcept;

  std::once_flag start_once_;
  void* value_ = nullptr;
  const std::chrono::milliseconds submission_timeout_;
  const ModelStreamerSessionOptions options_;
  const std::exception_ptr stopped_error_;
  utils::EventLoop event_loop_;
  std::unordered_map<std::uint64_t, std::shared_ptr<StreamerEntry>> active_;
  std::deque<std::shared_ptr<StreamerEntry>> pending_;
  uintmax_t session_bytes_ = 0;
  std::size_t session_submissions_ = 0;
  std::size_t unfinished_ = 0;
  bool work_scheduled_ = false;
  std::atomic<bool> failed_ = false;
};
}  // namespace snapshot::pagebroker

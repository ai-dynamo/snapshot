// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <condition_variable>
#include <deque>
#include <exception>
#include <functional>
#include <memory>
#include <mutex>
#include <thread>

namespace snapshot::pagebroker::utils {
class Event {
 public:
  virtual ~Event() = default;

  // Execute runs on the event-loop thread. An exception terminates the loop.
  virtual void Execute() = 0;

  // Cancel releases any waiter when this event cannot execute.
  virtual void Cancel(std::exception_ptr error) noexcept = 0;
};

// The owner must serialize Start(), Stop(), and destruction. These operations
// must not be called from event or failure callbacks. Post() may run concurrently
// with Start() and Stop(), but its callers must finish before destruction.
class EventLoop {
 public:
  using FailureHandler = std::function<void(std::exception_ptr)>;

  explicit EventLoop(FailureHandler failure_handler = {});
  ~EventLoop();
  EventLoop(const EventLoop&) = delete;
  EventLoop& operator=(const EventLoop&) = delete;

  void Start();
  // Post transfers ownership even when it rejects and cancels the event.
  bool Post(std::unique_ptr<Event> event);
  // Stop cancels queued events and waits for the executing event to return.
  // Repeated calls are allowed, including before Start().
  void Stop() noexcept;

 private:
  enum class State {
    CREATED,
    RUNNING,
    STOPPING,
    STOPPED,
    FAILED,
  };

  void Run() noexcept;
  void Fail(std::unique_ptr<Event> event, std::exception_ptr error) noexcept;
  static void CancelAll(std::deque<std::unique_ptr<Event>> events, std::exception_ptr error) noexcept;

  FailureHandler failure_handler_;
  std::mutex mutex_;
  std::condition_variable ready_;
  std::deque<std::unique_ptr<Event>> events_;
  std::thread worker_;
  std::exception_ptr terminal_error_;
  State state_ = State::CREATED;
};
}  // namespace snapshot::pagebroker::utils

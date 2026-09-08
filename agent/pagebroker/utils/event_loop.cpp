#include "event_loop.hpp"

#include <stdexcept>
#include <utility>

namespace snapshot::pagebroker::utils {
namespace {
std::exception_ptr
StoppedError()
{
  return std::make_exception_ptr(std::runtime_error("event loop stopped"));
}
}  // namespace

EventLoop::EventLoop(FailureHandler failure_handler)
    : failure_handler_(std::move(failure_handler))
{
}

EventLoop::~EventLoop()
{
  Stop();
}

void
EventLoop::Start()
{
  std::lock_guard lock(mutex_);
  if (state_ != State::CREATED)
    throw std::logic_error("event loop can only be started once");

  state_ = State::RUNNING;
  try {
    worker_ = std::thread([this] { Run(); });
  }
  catch (...) {
    state_ = State::CREATED;
    throw;
  }
}

bool
EventLoop::Post(std::unique_ptr<Event> event)
{
  if (!event)
    throw std::invalid_argument("event is required");

  std::exception_ptr error;
  {
    std::lock_guard lock(mutex_);
    if (state_ == State::RUNNING) {
      events_.push_back(std::move(event));
      ready_.notify_one();
      return true;
    }
    error = terminal_error_ ? terminal_error_ : StoppedError();
  }

  event->Cancel(std::move(error));
  return false;
}

void
EventLoop::Stop() noexcept
{
  std::lock_guard stop_lock(stop_mutex_);
  std::deque<std::unique_ptr<Event>> events;
  std::exception_ptr error;
  {
    std::lock_guard lock(mutex_);
    if (state_ == State::CREATED || state_ == State::RUNNING) {
      state_ = State::STOPPING;
      events.swap(events_);
    }
    error = terminal_error_ ? terminal_error_ : StoppedError();
  }

  CancelAll(std::move(events), std::move(error));
  ready_.notify_one();

  if (!worker_.joinable()) {
    std::lock_guard lock(mutex_);
    if (state_ == State::STOPPING)
      state_ = State::STOPPED;
    return;
  }
  if (worker_.get_id() == std::this_thread::get_id())
    return;
  try {
    worker_.join();
  }
  catch (...) {
    std::terminate();
  }
}

void
EventLoop::Run() noexcept
{
  while (true) {
    std::unique_ptr<Event> event;
    {
      std::unique_lock lock(mutex_);
      ready_.wait(lock, [this] { return state_ != State::RUNNING || !events_.empty(); });
      if (state_ != State::RUNNING)
        break;
      event = std::move(events_.front());
      events_.pop_front();
    }

    try {
      event->Execute();
    }
    catch (...) {
      Fail(std::move(event), std::current_exception());
      return;
    }
  }

  std::lock_guard lock(mutex_);
  if (state_ == State::STOPPING)
    state_ = State::STOPPED;
}

void
EventLoop::Fail(std::unique_ptr<Event> event, std::exception_ptr error) noexcept
{
  std::deque<std::unique_ptr<Event>> events;
  {
    std::lock_guard lock(mutex_);
    state_ = State::FAILED;
    terminal_error_ = error;
    events.swap(events_);
  }

  if (failure_handler_) {
    try {
      failure_handler_(error);
    }
    catch (...) {
    }
  }

  event->Cancel(error);
  CancelAll(std::move(events), std::move(error));
}

void
EventLoop::CancelAll(std::deque<std::unique_ptr<Event>> events, std::exception_ptr error) noexcept
{
  for (auto& event : events)
    event->Cancel(error);
}
}  // namespace snapshot::pagebroker::utils

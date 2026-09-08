#include "event_loop.hpp"

#include <gtest/gtest.h>

#include <atomic>
#include <exception>
#include <future>
#include <functional>
#include <memory>
#include <stdexcept>
#include <thread>
#include <utility>
#include <vector>

namespace snapshot::pagebroker::utils {
namespace {
class TestEvent final : public Event {
 public:
  using ExecuteFunction = std::function<void()>;
  using CancelFunction = std::function<void(std::exception_ptr)>;

  explicit TestEvent(ExecuteFunction execute, CancelFunction cancel = {})
      : execute_(std::move(execute)), cancel_(std::move(cancel))
  {
  }

  void Execute() override { execute_(); }

  void Cancel(std::exception_ptr error) noexcept override
  {
    if (!cancel_)
      return;
    try {
      cancel_(std::move(error));
    }
    catch (...) {
    }
  }

 private:
  ExecuteFunction execute_;
  CancelFunction cancel_;
};

TEST(EventLoopTest, ExecutesEventsInPostingOrder)
{
  EventLoop loop;
  loop.Start();

  std::vector<int> order;
  std::promise<void> completed;
  loop.Post(std::make_unique<TestEvent>([&] { order.push_back(1); }));
  loop.Post(std::make_unique<TestEvent>([&] { order.push_back(2); }));
  loop.Post(std::make_unique<TestEvent>([&] {
    order.push_back(3);
    completed.set_value();
  }));

  completed.get_future().get();
  loop.Stop();
  EXPECT_EQ(order, (std::vector<int>{1, 2, 3}));
}

TEST(EventLoopTest, AllowsAnEventToPostMoreWork)
{
  EventLoop loop;
  loop.Start();

  std::atomic<int> executions = 0;
  std::promise<void> completed;
  auto post = std::make_shared<std::function<void()>>();
  *post = [&] {
    loop.Post(std::make_unique<TestEvent>([&, post] {
      if (++executions == 3) {
        completed.set_value();
        return;
      }
      (*post)();
    }));
  };

  (*post)();
  completed.get_future().get();
  loop.Stop();
  EXPECT_EQ(executions, 3);
}

TEST(EventLoopTest, AcceptsConcurrentProducers)
{
  EventLoop loop;
  loop.Start();

  constexpr int kProducerCount = 4;
  constexpr int kEventsPerProducer = 100;
  std::atomic<int> executions = 0;
  std::vector<std::thread> producers;
  for (int producer = 0; producer < kProducerCount; ++producer) {
    producers.emplace_back([&] {
      for (int event = 0; event < kEventsPerProducer; ++event)
        loop.Post(std::make_unique<TestEvent>([&] { ++executions; }));
    });
  }
  for (auto& producer : producers)
    producer.join();

  std::promise<void> completed;
  loop.Post(std::make_unique<TestEvent>([&] { completed.set_value(); }));
  completed.get_future().get();
  loop.Stop();
  EXPECT_EQ(executions, kProducerCount * kEventsPerProducer);
}

TEST(EventLoopTest, StopCancelsQueuedEvents)
{
  EventLoop loop;
  loop.Start();

  std::promise<void> executing;
  std::promise<void> release;
  auto released = release.get_future().share();
  loop.Post(std::make_unique<TestEvent>([&] {
    executing.set_value();
    released.wait();
  }));
  executing.get_future().get();

  std::promise<void> cancelled;
  loop.Post(std::make_unique<TestEvent>(
      [] { FAIL() << "cancelled event was executed"; },
      [&](std::exception_ptr error) {
        EXPECT_NE(error, nullptr);
        cancelled.set_value();
      }));

  std::thread stop([&] { loop.Stop(); });
  cancelled.get_future().get();
  release.set_value();
  stop.join();
}

TEST(EventLoopTest, FailureCancelsCurrentAndQueuedEvents)
{
  std::promise<void> failure_handled;
  EventLoop loop([&](std::exception_ptr error) {
    EXPECT_NE(error, nullptr);
    failure_handled.set_value();
  });
  loop.Start();

  std::promise<void> current_cancelled;
  std::promise<void> queued_cancelled;
  loop.Post(std::make_unique<TestEvent>(
      [] { throw std::runtime_error("event failed"); },
      [&](std::exception_ptr error) {
        EXPECT_NE(error, nullptr);
        current_cancelled.set_value();
      }));
  loop.Post(std::make_unique<TestEvent>(
      [] { FAIL() << "queued event was executed"; },
      [&](std::exception_ptr error) {
        EXPECT_NE(error, nullptr);
        queued_cancelled.set_value();
      }));

  failure_handled.get_future().get();
  current_cancelled.get_future().get();
  queued_cancelled.get_future().get();
  loop.Stop();
}
}  // namespace
}  // namespace snapshot::pagebroker::utils

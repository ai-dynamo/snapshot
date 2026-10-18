// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "cancellation.hpp"

#include <gtest/gtest.h>

#include <chrono>
#include <condition_variable>
#include <future>
#include <latch>
#include <thread>

namespace snapshot::pagebroker {
using namespace std::chrono_literals;

TEST(CancellationTest, CopiesAndDeadlineViewsShareExplicitStop)
{
  Cancellation root;
  auto copy = root;
  auto view = root.WithDeadline(Cancellation::Clock::now() + 1h);
  root.Cancel();
  EXPECT_TRUE(copy.IsCancelled());
  EXPECT_TRUE(view.Token().stop_requested());
}

TEST(CancellationTest, DeadlineViewDoesNotStopParentOrSibling)
{
  Cancellation root;
  auto sibling = root.WithDeadline(Cancellation::Clock::now() + 1h);
  auto expired = root.WithDeadline(Cancellation::Clock::now());
  EXPECT_EQ(expired.Reason(), CancellationError::Reason::DEADLINE_EXCEEDED);
  EXPECT_FALSE(expired.Token().stop_requested());
  EXPECT_FALSE(root.IsCancelled());
  EXPECT_FALSE(sibling.IsCancelled());
  EXPECT_EQ(expired.WithDeadline(Cancellation::Clock::now() + 1h).Deadline(), expired.Deadline());
}

TEST(CancellationTest, ChildCancellationDoesNotReachParentOrSibling)
{
  Cancellation root;
  auto first = Cancellation::ChildOf(root);
  auto second = Cancellation::ChildOf(root);
  first.Cancel();
  EXPECT_TRUE(first.IsCancelled());
  EXPECT_FALSE(root.IsCancelled());
  EXPECT_FALSE(second.IsCancelled());
  root.Cancel();
  EXPECT_TRUE(second.IsCancelled());
}

TEST(CancellationTest, RetainsAncestorRegistrationsAfterIntermediateDestruction)
{
  Cancellation root;
  Cancellation grandchild;
  {
    auto child = Cancellation::ChildOf(root);
    grandchild = Cancellation::ChildOf(child);
  }
  EXPECT_FALSE(grandchild.IsCancelled());
  root.Cancel();
  EXPECT_TRUE(grandchild.Token().stop_requested());
}

TEST(CancellationTest, JoinRetainsAncestorAndEitherParentCanStopIt)
{
  for (const bool stop_first : {false, true}) {
    Cancellation ancestor, other;
    Cancellation joined;
    {
      auto intermediate = Cancellation::ChildOf(ancestor);
      joined = Cancellation::Join(intermediate, other);
    }
    if (stop_first)
      ancestor.Cancel();
    else
      other.Cancel();
    EXPECT_TRUE(joined.Token().stop_requested());
    EXPECT_EQ(ancestor.IsCancelled(), stop_first);
    EXPECT_EQ(other.IsCancelled(), !stop_first);
  }
}

TEST(CancellationTest, AlreadyStoppedParentStopsNewChildAndJoin)
{
  Cancellation stopped, other;
  stopped.Cancel();
  EXPECT_TRUE(Cancellation::ChildOf(stopped).Token().stop_requested());
  EXPECT_TRUE(Cancellation::Join(other, stopped).Token().stop_requested());
  EXPECT_TRUE(Cancellation::Join(stopped, other).Token().stop_requested());
  EXPECT_FALSE(other.IsCancelled());
}

TEST(CancellationTest, DeadlineWinsWhenBothConditionsHold)
{
  Cancellation root(Cancellation::Clock::now());
  root.Cancel();
  EXPECT_THROW({
    try {
      root.ThrowIfCancelled();
    } catch (const CancellationError& error) {
      EXPECT_EQ(error.reason, CancellationError::Reason::DEADLINE_EXCEEDED);
      throw;
    }
  }, CancellationError);
}

TEST(CancellationTest, ParentStopWakesChildTimedWait)
{
  Cancellation root;
  auto child = Cancellation::ChildOf(root);
  std::condition_variable_any changed;
  std::mutex mutex;
  std::latch started(1);
  auto waiter = std::async(std::launch::async, [&] {
    std::unique_lock lock(mutex);
    started.count_down();
    changed.wait_until(lock, child.Token(), Cancellation::Clock::now() + 1h,
        [&] { return child.IsCancelled(); });
    return child.IsCancelled();
  });
  started.wait();
  root.Cancel();
  EXPECT_EQ(waiter.wait_for(2s), std::future_status::ready);
  EXPECT_TRUE(waiter.get());
}
}  // namespace snapshot::pagebroker

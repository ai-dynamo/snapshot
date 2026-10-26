// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "tests/broker_transfer_fixture.hpp"

#include <chrono>
#include <filesystem>
#include <fstream>
#include <future>
#include <memory>
#include <mutex>
#include <string>
#include <thread>
#include <variant>

namespace snapshot::pagebroker {
using test::Boundary;

TEST_F(BrokerTransferTest, RestoreCommitSucceedsWhileCleanupIsPending)
{
  for (const bool limited : {false, true}) {
    SCOPED_TRACE(limited);
    Broker broker(staging_, storage_);
    const auto resources = limited ? Limit(broker) : nullptr;
    const auto transaction = SeedRestore(broker, Transaction::State::STAGED);
    BlockCleanup();
    auto request = RequestFor("seed");
    request.mutable_commit();
    EXPECT_TRUE(broker.HandleRequest(request).has_commit_complete());
    EXPECT_EQ(transaction->state(), Transaction::State::COMMITTED);
    EXPECT_TRUE(broker.HandleRequest(request).has_commit_complete());
    broker.ReapExpiredTransactions(std::chrono::steady_clock::now() + std::chrono::hours(3));
    EXPECT_EQ(transaction->state(), Transaction::State::COMMITTED);
    EXPECT_FALSE(transaction->TryClearDescriptor());
    EXPECT_TRUE(fs::exists(staging_ / "held/seed/image"));
    EXPECT_TRUE(std::holds_alternative<RestoreTransactionDescriptor>(transaction->descriptor()));
    EXPECT_EQ(Find(broker, "seed"), transaction);
    EXPECT_EQ(Retained(broker), 0u);
    if (limited) {
      EXPECT_EQ(resources->active, 1u);
      EXPECT_EQ(resources->reserved, 4u);
    }

    UnblockCleanup();
    broker.ReapExpiredTransactions(std::chrono::steady_clock::now());
    EXPECT_EQ(transaction->state(), Transaction::State::COMMITTED);
    EXPECT_TRUE(std::holds_alternative<std::monostate>(transaction->descriptor()));
    EXPECT_FALSE(fs::exists(staging_ / "restore/seed"));
    EXPECT_EQ(Retained(broker), 1u);
    EXPECT_TRUE(broker.HandleRequest(request).has_commit_complete());
    broker.ReapExpiredTransactions(std::chrono::steady_clock::now());
    EXPECT_EQ(Retained(broker), 1u);
    if (limited) {
      EXPECT_EQ(resources->active, 0u);
      EXPECT_EQ(resources->reserved, 0u);
    }
  }
}

TEST_F(BrokerTransferTest, FailedCleanupCannotBeEvictedWithoutResourceLimits)
{
  for (const auto state : {Transaction::State::COMMITTED, Transaction::State::ABORTED}) {
    SCOPED_TRACE(static_cast<int>(state));
    Broker broker(staging_, storage_);
    const auto transaction = SeedRestore(broker, state);
    BlockCleanup();
    auto request = RequestFor("seed");
    if (state == Transaction::State::COMMITTED)
      request.mutable_commit();
    else
      request.mutable_abort();
    const auto response = broker.HandleRequest(request);
    if (state == Transaction::State::COMMITTED) {
      EXPECT_TRUE(response.has_commit_complete());
    } else {
      EXPECT_EQ(response.failure().code(), Failure::STORAGE_ERROR);
    }
    EXPECT_FALSE(transaction->TryClearDescriptor());
    EXPECT_TRUE(std::holds_alternative<RestoreTransactionDescriptor>(transaction->descriptor()));
    EXPECT_EQ(Retained(broker), 0u);
    const auto sweep = std::chrono::steady_clock::now() + std::chrono::hours(2);
    broker.ReapExpiredTransactions(sweep);
    EXPECT_EQ(Find(broker, "seed"), transaction);
    UnblockCleanup();
    broker.ReapExpiredTransactions(std::chrono::steady_clock::now());
    EXPECT_FALSE(fs::exists(staging_ / "restore/seed"));
    EXPECT_EQ(Retained(broker), 1u);
    EXPECT_TRUE(std::holds_alternative<std::monostate>(transaction->descriptor()));
    const auto retry = broker.HandleRequest(request);
    EXPECT_TRUE(retry.has_commit_complete() || retry.has_abort_complete());
    broker.ReapExpiredTransactions(sweep);
    EXPECT_EQ(Find(broker, "seed"), nullptr);
  }
}

TEST_F(BrokerTransferTest, TerminalEvictionPreservesReusedTransactionID)
{
  Broker broker(staging_, storage_);
  const auto original = SeedRestore(broker, Transaction::State::STAGED);
  auto commit = RequestFor("seed");
  commit.mutable_commit();
  ASSERT_TRUE(broker.HandleRequest(commit).has_commit_complete());
  ASSERT_EQ(Retained(broker), 1u);

  // Retention still holds the old instance after its registry entry is replaced.
  Erase(broker, "seed", original);
  ASSERT_EQ(Find(broker, "seed"), nullptr);
  const auto replacement = Unprepared(broker, "seed");
  ASSERT_NE(replacement, original);

  broker.ReapExpiredTransactions(std::chrono::steady_clock::now() + std::chrono::hours(2));
  EXPECT_EQ(Retained(broker), 0u);
  EXPECT_EQ(Find(broker, "seed"), replacement);
  EXPECT_EQ(replacement->state(), Transaction::State::NEW);
}

TEST_F(BrokerTransferTest, PeriodicSweepRetainsMissedCompletionsAndEvictsThemWithoutRequests)
{
  for (const auto state : {Transaction::State::COMMITTED, Transaction::State::ABORTED}) {
    SCOPED_TRACE(static_cast<int>(state));
    Broker broker(staging_, storage_);
    auto transaction = SeedRestore(broker, state);
    {
      std::lock_guard lock(transaction->mutex());
      // Another request owns the mutex when completion tries to retain the result.
      std::async(std::launch::async, [&] { TryRetention(broker); }).get();
      std::async(std::launch::async, [&] { broker.ReapExpiredTransactions(std::chrono::steady_clock::now()); }).get();
      EXPECT_EQ(Retained(broker), 0u);
    }
    broker.ReapExpiredTransactions(std::chrono::steady_clock::now());
    EXPECT_EQ(Retained(broker), 1u);
    broker.ReapExpiredTransactions(std::chrono::steady_clock::now());
    EXPECT_EQ(Retained(broker), 1u);
    broker.ReapExpiredTransactions(std::chrono::steady_clock::now() + std::chrono::hours(2));
    EXPECT_EQ(Retained(broker), 0u);
    EXPECT_EQ(Find(broker, "seed"), nullptr);
  }
}

TEST_F(BrokerTransferTest, ExpiryReclaimsReservationsAndRestoresAdmission)
{
  Broker broker(staging_, storage_);
  SetLifetime(broker, std::chrono::minutes(1));
  const auto resources = Limit(broker);
  auto transaction = SeedRestore(broker, Transaction::State::STAGED);
  const auto sweep = std::chrono::steady_clock::now() + std::chrono::minutes(2);
  broker.ReapExpiredTransactions(sweep);
  EXPECT_EQ(transaction->state(), Transaction::State::ABORTED);
  EXPECT_TRUE(transaction->cancellation().Token().stop_requested());
  EXPECT_FALSE(fs::exists(staging_ / "restore/seed"));
  EXPECT_EQ(resources->active, 0u);
  EXPECT_EQ(resources->reserved, 0u);
  EXPECT_EQ(Retained(broker), 0u);
  EXPECT_EQ(Find(broker, "seed"), nullptr);
  // Another sweep cannot double-release capacity or retain a reaped transaction.
  broker.ReapExpiredTransactions(sweep);
  EXPECT_EQ(resources->active, 0u);
  EXPECT_EQ(Retained(broker), 0u);
  Transaction next;
  ASSERT_NO_THROW(next.PrepareTransfer(resources, 0));
  EXPECT_NO_THROW(next.Reserve(4));
  auto abort = RequestFor("seed");
  abort.mutable_abort();
  EXPECT_EQ(broker.HandleRequest(abort).failure().code(), Failure::TRANSACTION_NOT_FOUND);
  EXPECT_EQ(broker.HandleRequest(abort).failure().code(), Failure::TRANSACTION_NOT_FOUND);
}

TEST_F(BrokerTransferTest, PeriodicCleanupRetriesKeepCapacityUntilRemovalSucceeds)
{
  for (const auto state : {Transaction::State::STAGED, Transaction::State::COMMITTED, Transaction::State::ABORTED}) {
    SCOPED_TRACE(static_cast<int>(state));
    Broker broker(staging_, storage_);
    SetLifetime(broker, std::chrono::minutes(1));
    const auto resources = Limit(broker);
    auto transaction = SeedRestore(broker, state);
    BlockCleanup();
    const auto sweep = std::chrono::steady_clock::now() + std::chrono::minutes(2);
    for (unsigned retry = 0; retry < 2; ++retry) {
      broker.ReapExpiredTransactions(sweep);
      EXPECT_EQ(transaction->state(), state == Transaction::State::STAGED ? Transaction::State::ABORTING : state);
      EXPECT_TRUE(std::holds_alternative<RestoreTransactionDescriptor>(transaction->descriptor()));
      EXPECT_EQ(resources->active, 1u);
      EXPECT_EQ(resources->reserved, 4u);
      EXPECT_EQ(Retained(broker), 0u);
      Transaction next;
      EXPECT_THROW(next.PrepareTransfer(resources, 0), TransferError);
    }
    UnblockCleanup();
    broker.ReapExpiredTransactions(sweep);
    EXPECT_FALSE(fs::exists(staging_ / "restore/seed"));
    EXPECT_EQ(resources->active, 0u);
    EXPECT_EQ(resources->reserved, 0u);
    EXPECT_EQ(Retained(broker), state == Transaction::State::STAGED ? 0u : 1u);
    EXPECT_TRUE(std::holds_alternative<std::monostate>(transaction->descriptor()));
  }
}

TEST_F(BrokerTransferTest, AbortBeforePreparationSucceedsAndPreventsTransfer)
{
  for (const bool checkpoint : {false, true}) {
    for (const bool direct : {false, true}) {
      SCOPED_TRACE(checkpoint);
      SCOPED_TRACE(direct);
      Broker broker(staging_, storage_);
      const auto resources = Limit(broker);
      Intercept(broker, [](Boundary boundary, Cancellation) {
        ADD_FAILURE() << "aborted preparation reached " << test::BoundaryName(boundary);
      });
      auto prepare = checkpoint ? Checkpoint(direct) : Restore();
      if (!checkpoint && direct) {
        test::Configure(prepare.mutable_direct_restore()->mutable_source(),
            prepare.mutable_direct_restore()->mutable_io_engine(), source_);
      }
      // Model prepare registering its entry before it acquires the transaction lock.
      const auto transaction = Unprepared(broker, prepare.transaction_id());
      ASSERT_EQ(transaction->state(), Transaction::State::NEW);
      auto abort = RequestFor(prepare.transaction_id());
      abort.mutable_abort();
      EXPECT_TRUE(broker.HandleRequest(abort).has_abort_complete());
      EXPECT_EQ(transaction->state(), Transaction::State::ABORTED);
      EXPECT_TRUE(transaction->cancellation().Token().stop_requested());
      EXPECT_TRUE(broker.HandleRequest(abort).has_abort_complete());

      const auto response = broker.HandleRequest(prepare);
      ASSERT_TRUE(response.has_failure());
      EXPECT_EQ(response.failure().code(), Failure::TRANSACTION_CONFLICT);
      EXPECT_EQ(Find(broker, prepare.transaction_id()), transaction);
      EXPECT_EQ(transaction->state(), Transaction::State::ABORTED);
      EXPECT_EQ(Retained(broker), 1u);
      EXPECT_EQ(resources->active, 0u);
      EXPECT_EQ(resources->reserved, 0u);
      EXPECT_TRUE(std::holds_alternative<std::monostate>(transaction->descriptor()));
      EXPECT_TRUE(fs::is_empty(staging_ / "restore"));
      EXPECT_TRUE(fs::is_empty(staging_ / "checkpoint"));
      EXPECT_FALSE(fs::exists(CheckpointOutput(true)));
      EXPECT_TRUE(fs::exists(source_ / "image"));
    }
  }
}

TEST_F(BrokerTransferTest, AbortCleanupFailureRetainsStateAndAllowsRetry)
{
  for (const bool limited : {false, true}) {
    for (const auto state : {Transaction::State::STAGED, Transaction::State::ABORTED}) {
      SCOPED_TRACE(limited);
      SCOPED_TRACE(static_cast<int>(state));
      Broker broker(staging_, storage_);
      const auto resources = limited ? Limit(broker) : nullptr;
      const auto transaction = SeedRestore(broker, state);
      BlockCleanup();
      auto request = RequestFor("seed");
      request.mutable_abort();
      const auto response = broker.HandleRequest(request);
      EXPECT_EQ(response.failure().code(), Failure::STORAGE_ERROR);
      EXPECT_EQ(transaction->state(), state == Transaction::State::STAGED ? Transaction::State::ABORTING : state);
      EXPECT_TRUE(std::holds_alternative<RestoreTransactionDescriptor>(transaction->descriptor()));
      EXPECT_EQ(Retained(broker), 0u);
      if (limited) {
        EXPECT_EQ(resources->active, 1u);
        EXPECT_EQ(resources->reserved, 4u);
      }
      UnblockCleanup();
      EXPECT_TRUE(broker.HandleRequest(request).has_abort_complete());
      EXPECT_EQ(transaction->state(), Transaction::State::ABORTED);
      EXPECT_TRUE(std::holds_alternative<std::monostate>(transaction->descriptor()));
      EXPECT_FALSE(fs::exists(staging_ / "restore/seed"));
      EXPECT_EQ(Retained(broker), 1u);
      if (limited) {
        EXPECT_EQ(resources->active, 0u);
        EXPECT_EQ(resources->reserved, 0u);
      }
    }
  }
}

TEST_F(BrokerTransferTest, AbortWaitsForGpuDrainBeforeRemovingStaging)
{
  Broker broker(staging_, storage_);
  const auto resources = Limit(broker);
  const auto transaction = SeedRestore(broker, Transaction::State::STAGED);
  const auto operation = std::make_shared<Transaction::GpuOperation>();
  operation->state = Transaction::GpuOperation::State::Running;
  operation->cancellation = std::make_shared<Cancellation>(Cancellation::ChildOf(transaction->cancellation()));
  transaction->gpu_operation = operation;
  auto request = RequestFor("seed");
  request.mutable_abort();
  auto abort = std::async(std::launch::async, [&] { return broker.HandleRequest(request); });

  // Observe the wait while owning the same mutex GPU completion needs. The
  // deadline bounds the observation; always finish the fake operation below.
  std::unique_lock lock(transaction->mutex(), std::defer_lock);
  bool waiting = false;
  const auto deadline = std::chrono::steady_clock::now() + std::chrono::seconds(2);
  while (std::chrono::steady_clock::now() < deadline) {
    if (lock.try_lock()) {
      if (transaction->state() == Transaction::State::ABORTING) {
        waiting = true;
        break;
      }
      lock.unlock();
    }
    std::this_thread::sleep_for(std::chrono::milliseconds(1));
  }
  EXPECT_TRUE(waiting);
  if (!lock.owns_lock())
    lock.lock(); // A broken wait can hold it only until the production timeout.
  EXPECT_EQ(abort.wait_for(std::chrono::milliseconds(0)), std::future_status::timeout);
  EXPECT_TRUE(operation->cancellation->IsCancelled());
  EXPECT_TRUE(fs::exists(staging_ / "restore/seed/image"));
  EXPECT_EQ(resources->active, 1u);
  EXPECT_EQ(resources->reserved, 4u);
  operation->state = Transaction::GpuOperation::State::Finished;
  operation->completed.notify_all();
  lock.unlock();
  const auto response = abort.get();
  EXPECT_TRUE(response.has_abort_complete()) << response.DebugString();
  EXPECT_EQ(transaction->state(), Transaction::State::ABORTED);
  EXPECT_FALSE(fs::exists(staging_ / "restore/seed"));
  EXPECT_EQ(resources->active, 0u);
  EXPECT_EQ(resources->reserved, 0u);
}

TEST_F(BrokerTransferTest, ExpiryWaitsForGpuDrainBeforeRemovingStaging)
{
  Broker broker(staging_, storage_);
  SetLifetime(broker, std::chrono::minutes(1));
  const auto resources = Limit(broker);
  auto transaction = SeedRestore(broker, Transaction::State::STAGED);
  auto operation = std::make_shared<Transaction::GpuOperation>();
  operation->state = Transaction::GpuOperation::State::Running;
  operation->cancellation = std::make_shared<Cancellation>(Cancellation::ChildOf(transaction->cancellation()));
  transaction->gpu_operation = operation;
  const auto sweep = std::chrono::steady_clock::now() + std::chrono::minutes(2);
  auto reap = std::async(std::launch::async, [&] { broker.ReapExpiredTransactions(sweep); });
  const auto ready = reap.wait_for(std::chrono::seconds(2));
  if (ready != std::future_status::ready) {
    // Release a regressed GPU wait before reporting the failure.
    std::lock_guard lock(transaction->mutex());
    operation->state = Transaction::GpuOperation::State::Finished;
    operation->completed.notify_all();
  }
  reap.get();
  ASSERT_EQ(ready, std::future_status::ready);
  EXPECT_TRUE(operation->cancellation->IsCancelled());
  EXPECT_EQ(transaction->state(), Transaction::State::ABORTING);
  EXPECT_TRUE(fs::exists(staging_ / "restore/seed/image"));
  EXPECT_EQ(resources->active, 1u);
  EXPECT_EQ(resources->reserved, 4u);
  broker.SetClock([sweep] { return sweep; });
  auto request = RequestFor("seed");
  request.mutable_commit();
  EXPECT_EQ(broker.HandleRequest(request).failure().code(), Failure::TRANSACTION_EXPIRED);
  request.mutable_abort();
  EXPECT_EQ(broker.HandleRequest(request).failure().code(), Failure::TRANSACTION_EXPIRED);
  EXPECT_TRUE(fs::exists(staging_ / "restore/seed/image"));
  operation->state = Transaction::GpuOperation::State::Finished;
  broker.ReapExpiredTransactions(sweep);
  EXPECT_FALSE(fs::exists(staging_ / "restore/seed"));
  EXPECT_EQ(resources->active, 0u);
  EXPECT_EQ(resources->reserved, 0u);
  EXPECT_EQ(broker.HandleRequest(request).failure().code(), Failure::TRANSACTION_NOT_FOUND);
}

TEST_F(BrokerTransferTest, ShutdownSignalsGpuWithoutWaitingForTransactionMutex)
{
  Broker broker(staging_, storage_);
  SetLifetime(broker, std::chrono::minutes(1));
  Limit(broker);
  auto transaction = SeedRestore(broker, Transaction::State::STAGED);
  auto operation = std::make_shared<Transaction::GpuOperation>();
  operation->state = Transaction::GpuOperation::State::Running;
  operation->cancellation = std::make_shared<Cancellation>(Cancellation::ChildOf(transaction->cancellation()));
  transaction->gpu_operation = operation;
  std::unique_lock lock(transaction->mutex());
  auto shutdown = std::async(std::launch::async, [&] {
    broker.Shutdown();
    broker.Shutdown();
  });
  const auto signalled = shutdown.wait_for(std::chrono::seconds(2));
  lock.unlock();
  shutdown.get();
  EXPECT_EQ(signalled, std::future_status::ready);
  EXPECT_TRUE(operation->cancellation->IsCancelled());
  EXPECT_TRUE(fs::exists(staging_ / "restore/seed/image"));
  operation->state = Transaction::GpuOperation::State::Finished;
  broker.ReapExpiredTransactions(std::chrono::steady_clock::now() + std::chrono::minutes(2));
  EXPECT_FALSE(fs::exists(staging_ / "restore/seed"));
}

TEST_F(BrokerTransferTest, ExpirySkipsActiveTransfersUntilTheyDrain)
{
  std::ofstream(source_ / "image") << "data";
  Broker broker(staging_, storage_);
  SetLifetime(broker, std::chrono::minutes(1));
  const auto resources = Limit(broker);
  std::promise<void> staged, resume;
  Intercept(broker, [&](Boundary boundary, Cancellation) {
    if (boundary == Boundary::StageRestore) {
      staged.set_value();
      resume.get_future().wait();
    }
  });
  const auto request = Restore();
  auto restore = std::async(std::launch::async, [&] { return broker.HandleRequest(request); });
  staged.get_future().wait();
  const auto sweep = std::chrono::steady_clock::now() + std::chrono::minutes(2);
  broker.ReapExpiredTransactions(sweep);
  EXPECT_TRUE(fs::exists(staging_ / "restore/restore/image"));
  EXPECT_EQ(resources->active, 1u);
  EXPECT_EQ(resources->reserved, 4u);
  resume.set_value();
  ASSERT_TRUE(restore.get().has_staged_restore_directory());
  broker.ReapExpiredTransactions(sweep);
  EXPECT_FALSE(fs::exists(staging_ / "restore/restore"));
  EXPECT_EQ(resources->active, 0u);
  EXPECT_EQ(resources->reserved, 0u);
}

TEST_F(BrokerTransferTest, ExpiredRequestsLeaveCleanupToTheReaper)
{
  for (const bool commit : {false, true}) {
    SCOPED_TRACE(commit ? "commit" : "abort");
    Broker broker(staging_, storage_);
    SetLifetime(broker, std::chrono::seconds(0));
    const auto resources = Limit(broker);
    const auto transaction = SeedRestore(broker, Transaction::State::STAGED);
    auto request = RequestFor("seed");
    if (commit)
      request.mutable_commit();
    else
      request.mutable_abort();

    BlockCleanup();
    EXPECT_EQ(broker.HandleRequest(request).failure().code(), Failure::TRANSACTION_EXPIRED);
    EXPECT_EQ(transaction->state(), Transaction::State::STAGED);
    broker.ReapExpiredTransactions(std::chrono::steady_clock::now());
    EXPECT_EQ(transaction->state(), Transaction::State::ABORTING);
    EXPECT_EQ(resources->active, 1u);
    EXPECT_EQ(resources->reserved, 4u);
    EXPECT_EQ(Retained(broker), 0u);

    UnblockCleanup();
    EXPECT_EQ(broker.HandleRequest(request).failure().code(), Failure::TRANSACTION_EXPIRED);
    EXPECT_TRUE(fs::exists(staging_ / "restore/seed/image"));
    broker.ReapExpiredTransactions(std::chrono::steady_clock::now());
    EXPECT_EQ(transaction->state(), Transaction::State::ABORTED);
    EXPECT_FALSE(fs::exists(staging_ / "restore/seed"));
    EXPECT_EQ(resources->active, 0u);
    EXPECT_EQ(resources->reserved, 0u);
    request.mutable_commit();
    EXPECT_EQ(broker.HandleRequest(request).failure().code(), Failure::TRANSACTION_NOT_FOUND);
    request.mutable_abort();
    EXPECT_EQ(broker.HandleRequest(request).failure().code(), Failure::TRANSACTION_NOT_FOUND);
  }
}
}  // namespace snapshot::pagebroker

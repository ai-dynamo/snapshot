// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "tests/broker_transfer_fixture.hpp"

#include <sys/stat.h>

#include <chrono>
#include <exception>
#include <filesystem>
#include <fstream>
#include <future>
#include <memory>
#include <mutex>
#include <stdexcept>
#include <stop_token>
#include <string>
#include <thread>
#include <variant>
#include <vector>

namespace snapshot::pagebroker {
using test::Boundary;
using test::BoundaryName;
using test::Configure;
using test::ScopedMkdirHook;

TEST_F(BrokerTransferTest, DirectRestoreUsesGenericPreparationAndBorrowsItsSource)
{
  Broker broker(staging_, storage_);
  SetLifetime(broker, std::chrono::hours(1));
  auto resources = Limit(broker);
  ASSERT_EQ(mkfifo((source_ / "unscanned").c_str(), 0600), 0);
  std::vector<Boundary> calls;
  Intercept(broker, [&](Boundary boundary, Cancellation) { calls.push_back(boundary); });
  auto request = RequestFor("direct");
  Configure(request.mutable_direct_restore()->mutable_source(), request.mutable_direct_restore()->mutable_io_engine(),
      source_);
  ASSERT_TRUE(broker.HandleRequest(request).has_direct_restore_ready());
  EXPECT_EQ(calls, (std::vector<Boundary>{Boundary::PrepareRestore, Boundary::StageRestore}));
  EXPECT_EQ(resources->reserved, 0u);
  const auto transaction = Find(broker, "direct");
  const auto& access = std::get<RestoreTransactionDescriptor>(transaction->descriptor());
  EXPECT_TRUE(access.staging_directory().empty());
  struct stat original{}, retained{};
  ASSERT_EQ(stat(source_.c_str(), &original), 0);
  ASSERT_EQ(fstat(access.source_fd(), &retained), 0);
  EXPECT_EQ(original.st_ino, retained.st_ino);
  request.mutable_abort();
  ASSERT_TRUE(broker.HandleRequest(request).has_abort_complete());
  EXPECT_TRUE(fs::exists(source_ / "image"));
}

TEST_F(BrokerTransferTest, DirectRestoreAbortPreservesUnownedDirectories)
{
  Broker broker(staging_, storage_);
  auto request = RequestFor("direct");
  Configure(request.mutable_direct_restore()->mutable_source(), request.mutable_direct_restore()->mutable_io_engine(),
      source_);
  ASSERT_TRUE(broker.HandleRequest(request).has_direct_restore_ready());

  // Matching names are not ownership: direct restore never created these trees.
  ASSERT_TRUE(fs::create_directory(staging_ / "restore/direct"));
  ASSERT_TRUE(fs::create_directory(staging_ / "checkpoint/direct"));
  request.mutable_abort();
  ASSERT_TRUE(broker.HandleRequest(request).has_abort_complete());
  EXPECT_TRUE(fs::exists(staging_ / "restore/direct"));
  EXPECT_TRUE(fs::exists(staging_ / "checkpoint/direct"));
}

TEST_F(BrokerTransferTest, FailedDirectRestoreNeverRemovesBorrowedData)
{
  Broker broker(staging_, storage_);
  Intercept(broker, [&](Boundary boundary, Cancellation) {
    if (boundary == Boundary::StageRestore)
      throw std::runtime_error("access failed");
  });
  auto request = RequestFor("direct-failure");
  Configure(request.mutable_direct_restore()->mutable_source(), request.mutable_direct_restore()->mutable_io_engine(),
      source_);
  EXPECT_EQ(broker.HandleRequest(request).failure().code(), Failure::STORAGE_ERROR);
  request.mutable_abort();
  EXPECT_TRUE(broker.HandleRequest(request).has_abort_complete());
  EXPECT_TRUE(fs::exists(source_ / "image"));
}

TEST_F(BrokerTransferTest, PreservesFailureCodesAcrossRequestAndTransferBoundaries)
{
  const struct {
    std::exception_ptr error;
    Failure::Code code;
  } failures[] = {
      {std::make_exception_ptr(CancellationError(CancellationError::Reason::CANCELLED)), Failure::INTERNAL_ERROR},
      {std::make_exception_ptr(CancellationError(CancellationError::Reason::DEADLINE_EXCEEDED)), Failure::TRANSACTION_EXPIRED},
      {std::make_exception_ptr(TransferError(Failure::OUTCOME_UNKNOWN, "publication uncertain")), Failure::OUTCOME_UNKNOWN},
      {std::make_exception_ptr(std::runtime_error("storage failed")), Failure::STORAGE_ERROR},
  };
  for (const auto& failure : failures) {
    SCOPED_TRACE(Failure::Code_Name(failure.code));
    for (const auto boundary : {Boundary::PrepareRestore, Boundary::StageRestore, Boundary::ValidateCheckpoint,
                               Boundary::InspectCheckpoint, Boundary::PublishCheckpoint}) {
      SCOPED_TRACE(BoundaryName(boundary));
      Broker broker(staging_, storage_);
      Intercept(broker, [&](Boundary current, Cancellation) {
        if (current == boundary)
          std::rethrow_exception(failure.error);
      });
      if (boundary == Boundary::PrepareRestore || boundary == Boundary::StageRestore) {
        const auto response = broker.HandleRequest(Restore());
        ASSERT_TRUE(response.has_failure()) << response.DebugString();
        EXPECT_EQ(response.failure().code(), failure.code);
        EXPECT_FALSE(fs::exists(staging_ / "restore" / "restore"));
      } else {
        auto prepare = Checkpoint(false);
        auto response = broker.HandleRequest(prepare);
        if (boundary != Boundary::ValidateCheckpoint) {
          ASSERT_TRUE(response.has_staged_checkpoint_directory()) << response.DebugString();
          const Path directory(response.staged_checkpoint_directory().image_directory());
          std::ofstream(directory / "image") << "image";
          auto commit = RequestFor("checkpoint");
          commit.mutable_commit();
          response = broker.HandleRequest(commit);
          EXPECT_TRUE(fs::exists(directory / "image"));
          auto abort = RequestFor("checkpoint");
          abort.mutable_abort();
          EXPECT_TRUE(broker.HandleRequest(abort).has_abort_complete());
          EXPECT_FALSE(fs::exists(directory));
        }
        ASSERT_TRUE(response.has_failure()) << response.DebugString();
        EXPECT_EQ(response.failure().code(), failure.code);
        EXPECT_FALSE(fs::exists(storage_ / "published"));
      }
    }
  }
}

TEST_F(BrokerTransferTest, CleanupFailurePreservesTransferErrorAndRetainsReservation)
{
  std::ofstream(source_ / "image") << "data";
  for (const auto code : {Failure::TRANSACTION_EXPIRED, Failure::TRANSACTION_CONFLICT}) {
    SCOPED_TRACE(Failure::Code_Name(code));
    for (const bool limited : {false, true}) {
      SCOPED_TRACE(limited);
      Broker broker(staging_, storage_);
      const auto resources = limited ? Limit(broker) : nullptr;
      Intercept(broker, [&](Boundary boundary, Cancellation) {
        if (boundary != Boundary::StageRestore)
          return;
        BlockCleanup();
        if (code == Failure::TRANSACTION_EXPIRED)
          throw CancellationError(CancellationError::Reason::DEADLINE_EXCEEDED);
        throw TransferError(code, "destination conflict");
      });
      const auto response = broker.HandleRequest(Restore());
      ASSERT_TRUE(response.has_failure()) << response.DebugString();
      EXPECT_EQ(response.failure().code(), code);
      const std::string prefix = code == Failure::TRANSACTION_EXPIRED
          ? "operation deadline exceeded; cleanup: " : "destination conflict; cleanup: ";
      EXPECT_TRUE(response.failure().message().starts_with(prefix)) << response.DebugString();
      EXPECT_GT(response.failure().message().size(), prefix.size());
      const auto transaction = Find(broker, "restore");
      ASSERT_NE(transaction, nullptr);
      EXPECT_EQ(transaction->state(), Transaction::State::ABORTED);
      EXPECT_TRUE(fs::is_directory(staging_ / "held/restore"));
      if (resources) {
        EXPECT_EQ(resources->active, 1u);
        EXPECT_EQ(resources->reserved, 4u);
      }

      UnblockCleanup();
      broker.ReapExpiredTransactions(std::chrono::steady_clock::now());
      EXPECT_FALSE(fs::exists(staging_ / "restore/restore"));
      EXPECT_EQ(transaction->state(), Transaction::State::ABORTED);
      EXPECT_EQ(Retained(broker), 1u);
      EXPECT_EQ(fs::file_size(source_ / "image"), 4u);
      if (resources) {
        EXPECT_EQ(resources->active, 0u);
        EXPECT_EQ(resources->reserved, 0u);
      }
    }
  }
}

TEST_F(BrokerTransferTest, ShutdownRejectsNewTransactionsAsUnavailable)
{
  Broker broker(staging_, storage_);
  broker.Shutdown();
  broker.Shutdown(); // Serve and the destructor may both initiate shutdown.
  for (const auto& request : {Restore(), Checkpoint(false), Checkpoint(true)}) {
    SCOPED_TRACE(request.DebugString());
    const auto response = broker.HandleRequest(request);
    ASSERT_TRUE(response.has_failure()) << response.DebugString();
    EXPECT_EQ(response.failure().code(), Failure::UNAVAILABLE);
    EXPECT_EQ(response.failure().message(), "PageBroker is shutting down");
    EXPECT_EQ(Find(broker, request.transaction_id()), nullptr);
  }
  EXPECT_TRUE(fs::is_empty(staging_ / "restore"));
  EXPECT_TRUE(fs::is_empty(staging_ / "checkpoint"));
  EXPECT_FALSE(fs::exists(CheckpointOutput(true)));
}

TEST_F(BrokerTransferTest, ShutdownCancelsActiveRestoreAndRemovesStaging)
{
  Broker broker(staging_, storage_);
  std::promise<void> staged, resume;
  Intercept(broker, [&](Boundary boundary, Cancellation control) {
    if (boundary == Boundary::StageRestore) {
      staged.set_value();
      resume.get_future().wait();
      control.ThrowIfCancelled();
    }
  });
  const auto request = Restore();
  auto restore = std::async(std::launch::async, [&] { return broker.HandleRequest(request); });
  staged.get_future().wait();
  broker.Shutdown();
  resume.set_value();
  const auto response = restore.get();
  ASSERT_TRUE(response.has_failure()) << response.DebugString();
  EXPECT_EQ(response.failure().code(), Failure::INTERNAL_ERROR);
  EXPECT_EQ(response.failure().message(), "operation cancelled");
  EXPECT_FALSE(fs::exists(staging_ / "restore" / "restore"));
}

TEST_F(BrokerTransferTest, PreparationUsesBrokerLifetimeWithOrWithoutLimits)
{
  auto direct_restore = RequestFor("restore");
  Configure(direct_restore.mutable_direct_restore()->mutable_source(),
      direct_restore.mutable_direct_restore()->mutable_io_engine(), source_);
  for (const auto& request : {Restore(), direct_restore, Checkpoint(false), Checkpoint(true)}) {
    SCOPED_TRACE(request.DebugString());
    for (const bool limited : {false, true}) {
      SCOPED_TRACE(limited);
      Broker broker(staging_, storage_);
      if (limited)
        Limit(broker, 100);
      const auto lifetime = std::chrono::seconds{17};
      SetLifetime(broker, lifetime);
      const auto started = Cancellation::Clock::now();
      bool prepared = false;
      Intercept(broker, [&](Boundary boundary, Cancellation control) {
        if (boundary != Boundary::PrepareRestore && boundary != Boundary::ValidateCheckpoint)
          return;
        prepared = true;
        EXPECT_GE(control.Deadline(), started + lifetime);
        EXPECT_LE(control.Deadline(), Cancellation::Clock::now() + lifetime);
      });
      const auto response = broker.HandleRequest(request);
      ASSERT_FALSE(response.has_failure()) << response.DebugString();
      EXPECT_TRUE(prepared);
      auto abort = RequestFor(request.transaction_id());
      abort.mutable_abort();
      ASSERT_TRUE(broker.HandleRequest(abort).has_abort_complete());
    }
  }
}

TEST_F(BrokerTransferTest, PreparationUsesTheTransactionDeadline)
{
  Broker broker(staging_, storage_);
  SetLifetime(broker, std::chrono::seconds(1));
  Intercept(broker, [&](Boundary boundary, Cancellation control) {
    if (boundary != Boundary::PrepareRestore)
      return;
    EXPECT_NE(Find(broker, "restore"), nullptr);
    EXPECT_NE(control.Deadline(), Cancellation::Clock::time_point::max());
    if (control.Deadline() != Cancellation::Clock::time_point::max())
      std::this_thread::sleep_until(control.Deadline());
  });
  const auto response = broker.HandleRequest(Restore());
  EXPECT_EQ(response.failure().code(), Failure::TRANSACTION_EXPIRED) << response.DebugString();
  EXPECT_FALSE(fs::exists(staging_ / "restore/restore"));
  EXPECT_TRUE(fs::exists(source_ / "image"));
}

TEST_F(BrokerTransferTest, AbortAndShutdownCancelPreparation)
{
  for (const bool shutdown : {false, true}) {
    SCOPED_TRACE(shutdown);
    Broker broker(staging_, storage_);
    std::promise<void> preparing, resume, cancelled;
    auto resumed = resume.get_future();
    auto stopped = cancelled.get_future();
    std::stop_token token;
    Intercept(broker, [&](Boundary boundary, Cancellation control) {
      if (boundary == Boundary::PrepareRestore) {
        token = control.Token();
        preparing.set_value();
        resumed.wait();
      }
    });
    const auto restore_request = Restore();
    auto abort_request = RequestFor("restore");
    abort_request.mutable_abort();
    auto restore = std::async(std::launch::async, [&] { return broker.HandleRequest(restore_request); });
    preparing.get_future().wait();
    std::stop_callback on_stop(token, [&] { cancelled.set_value(); });
    auto cancel = std::async(std::launch::async, [&] {
      if (shutdown) {
        broker.Shutdown();
        return Response{};
      }
      return broker.HandleRequest(abort_request);
    });
    // Always release the worker, including on failure, so the test cannot deadlock.
    EXPECT_EQ(stopped.wait_for(std::chrono::seconds(1)), std::future_status::ready);
    resume.set_value();
    const auto response = restore.get();
    EXPECT_EQ(response.failure().code(), Failure::INTERNAL_ERROR) << response.DebugString();
    const auto result = cancel.get();
    if (!shutdown) {
      EXPECT_TRUE(result.has_abort_complete()) << result.DebugString();
    }
    EXPECT_FALSE(fs::exists(staging_ / "restore/restore"));
    EXPECT_TRUE(fs::exists(source_ / "image"));
  }
}

TEST_F(BrokerTransferTest, ExistingRestoreStagingRejectsWithoutRegisteringTransaction)
{
  Broker broker(staging_, storage_);
  const auto resources = Limit(broker, 16);
  // Create the conflict after broker startup, which cleans old staging trees.
  const Path directory = staging_ / "restore/restore";
  fs::create_directory(directory);
  std::ofstream(directory / "marker") << "keep";
  const auto permissions = fs::status(directory).permissions();
  Intercept(broker, [](Boundary, Cancellation) { ADD_FAILURE() << "conflict reached the transfer engine"; });

  const auto response = broker.HandleRequest(Restore());
  ASSERT_TRUE(response.has_failure());
  EXPECT_EQ(response.failure().code(), Failure::TRANSACTION_CONFLICT);
  EXPECT_EQ(Find(broker, "restore"), nullptr);
  EXPECT_EQ(fs::file_size(directory / "marker"), 4u);
  EXPECT_EQ(fs::status(directory).permissions(), permissions);
  EXPECT_EQ(resources->active, 0u);
  EXPECT_EQ(resources->reserved, 0u);
}

TEST_F(BrokerTransferTest, RestoreStagingConflictAbortsOldHandleAndAllowsFreshRetry)
{
  Broker broker(staging_, storage_);
  // Model another request that obtained the same handle before rejection.
  const auto rejected = Unprepared(broker, "restore");
  const Path directory = staging_ / "restore/restore";
  fs::create_directory(directory);
  EXPECT_EQ(broker.HandleRequest(Restore()).failure().code(), Failure::TRANSACTION_CONFLICT);
  {
    std::lock_guard lock(rejected->mutex());
    EXPECT_EQ(rejected->state(), Transaction::State::ABORTED);
  }
  EXPECT_EQ(Find(broker, "restore"), nullptr);

  fs::remove(directory);
  ASSERT_TRUE(broker.HandleRequest(Restore()).has_staged_restore_directory());
  const auto replacement = Find(broker, "restore");
  ASSERT_NE(replacement, nullptr);
  EXPECT_NE(replacement, rejected);
  EXPECT_EQ(replacement->state(), Transaction::State::STAGED);
  EXPECT_EQ(rejected->state(), Transaction::State::ABORTED);
  EXPECT_EQ(fs::file_size(directory / "image"), 5u);
}

TEST_F(BrokerTransferTest, DuplicateRestorePreservesRegisteredStagedTransaction)
{
  Broker broker(staging_, storage_);
  const auto resources = Limit(broker, 16);
  const auto request = Restore();
  ASSERT_TRUE(broker.HandleRequest(request).has_staged_restore_directory());
  const auto transaction = Find(broker, "restore");
  ASSERT_NE(transaction, nullptr);

  EXPECT_EQ(broker.HandleRequest(request).failure().code(), Failure::TRANSACTION_CONFLICT);
  EXPECT_EQ(Find(broker, "restore"), transaction);
  EXPECT_EQ(transaction->state(), Transaction::State::STAGED);
  EXPECT_EQ(fs::file_size(staging_ / "restore/restore/image"), 5u);
  EXPECT_EQ(resources->active, 1u);
  EXPECT_EQ(resources->reserved, 5u);

  auto commit = RequestFor("restore");
  commit.mutable_commit();
  EXPECT_TRUE(broker.HandleRequest(commit).has_commit_complete());
  EXPECT_EQ(resources->active, 0u);
  EXPECT_EQ(resources->reserved, 0u);
}

TEST_F(BrokerTransferTest, InvalidPreparationReleasesAdmissionAndAllowsCorrectedRequest)
{
  std::ofstream(source_ / "image") << "data";
  Broker broker(staging_, storage_);
  SetLifetime(broker, std::chrono::minutes(1));
  const auto resources = Limit(broker);
  auto request = Restore();
  request.mutable_staged_restore()->mutable_source()->mutable_filesystem()->set_directory("relative");
  EXPECT_EQ(broker.HandleRequest(request).failure().code(), Failure::INVALID_REQUEST);
  EXPECT_EQ(resources->active, 0u);
  EXPECT_EQ(resources->reserved, 0u);
  EXPECT_EQ(Find(broker, "restore"), nullptr);
  ASSERT_TRUE(broker.HandleRequest(Restore()).has_staged_restore_directory());
  request.mutable_abort();
  EXPECT_TRUE(broker.HandleRequest(request).has_abort_complete());
  EXPECT_EQ(resources->active, 0u);
  EXPECT_EQ(resources->reserved, 0u);
}

TEST_F(BrokerTransferTest, SpaceRejectionAbortsHeldTransactionAndReleasesAdmission)
{
  std::ofstream(source_ / "image") << "data";
  for (const bool limited : {false, true}) {
    SCOPED_TRACE(limited);
    Broker broker(staging_, storage_);
    const auto resources = limited ? Limit(broker) : nullptr;
    std::shared_ptr<Transaction> rejected;
    Intercept(broker, [&](Boundary boundary, Cancellation) {
      if (boundary == Boundary::PrepareRestore && !rejected)
        rejected = Find(broker, "restore");
    });
    // Simulate space promised to other copies without filling the filesystem.
    SetReservedStaging(broker, fs::space(staging_).capacity);
    const auto response = broker.HandleRequest(Restore());
    ASSERT_TRUE(response.has_failure()) << response.DebugString();
    EXPECT_EQ(response.failure().code(), Failure::INSUFFICIENT_STORAGE);
    ASSERT_NE(rejected, nullptr);
    {
      std::lock_guard lock(rejected->mutex());
      EXPECT_EQ(rejected->state(), Transaction::State::ABORTED);
    }
    EXPECT_EQ(Find(broker, "restore"), nullptr);
    if (resources) {
      EXPECT_EQ(resources->active, 0u);
      EXPECT_EQ(resources->reserved, 0u);
    }
    EXPECT_FALSE(fs::exists(staging_ / "restore/restore"));
    EXPECT_EQ(fs::file_size(source_ / "image"), 4u);

    // The same ID can be admitted while a waiter still owns the rejected handle.
    SetReservedStaging(broker, 0);
    const auto retry = broker.HandleRequest(Restore());
    ASSERT_TRUE(retry.has_staged_restore_directory()) << retry.DebugString();
    EXPECT_NE(Find(broker, "restore"), rejected);
    rejected.reset();
    if (resources) {
      EXPECT_EQ(resources->active, 1u);
      EXPECT_EQ(resources->reserved, 4u);
    }
    auto abort = RequestFor("restore");
    abort.mutable_abort();
    ASSERT_TRUE(broker.HandleRequest(abort).has_abort_complete());
    if (resources) {
      EXPECT_EQ(resources->active, 0u);
      EXPECT_EQ(resources->reserved, 0u);
    }
  }
}

TEST_F(BrokerTransferTest, CompletionRejectsExpiryAndCancellationAfterTransfer)
{
  for (const bool empty : {false, true}) {
    for (const bool expire : {false, true}) {
      SCOPED_TRACE(empty);
      SCOPED_TRACE(expire);
      if (empty)
        fs::remove(source_ / "image");
      Broker broker(staging_, storage_);
      SetLifetime(broker, std::chrono::seconds(1));
      Intercept(broker, [&](Boundary boundary, Cancellation control) {
        if (boundary != Boundary::StageRestore)
          return;
        // Simulate interruption as the final copy (or empty-directory setup) returns.
        if (expire)
          std::this_thread::sleep_until(control.Deadline());
        else
          Find(broker, "restore")->CancelWork();
      });
      const auto response = broker.HandleRequest(Restore());
      EXPECT_EQ(response.failure().code(), expire ? Failure::TRANSACTION_EXPIRED : Failure::INTERNAL_ERROR)
          << response.DebugString();
      EXPECT_FALSE(fs::exists(staging_ / "restore/restore"));
      EXPECT_TRUE(fs::exists(source_));
    }
  }
}

TEST_F(BrokerTransferTest, RestoreCreationRejectsEntriesAppearingDuringPreparation)
{
  for (const auto* kind : {"directory", "file", "live", "dangling"}) {
    SCOPED_TRACE(kind);
    Broker broker(staging_, storage_);
    const auto resources = Limit(broker, 100);
    const auto output = staging_ / "restore/restore";
    Intercept(broker, [&](Boundary boundary, Cancellation) {
      if (boundary != Boundary::PrepareRestore)
        return;
      if (std::string(kind) == "directory") {
        fs::create_directory(output);
        std::ofstream(output / "marker") << "keep";
      } else if (std::string(kind) == "file") {
        std::ofstream(output) << "keep";
      } else {
        fs::create_directory_symlink(std::string(kind) == "live" ? source_ : storage_ / "missing", output);
      }
    });
    const auto response = broker.HandleRequest(Restore());
    EXPECT_EQ(response.failure().code(), Failure::TRANSACTION_CONFLICT) << response.DebugString();
    EXPECT_EQ(resources->active, 0u);
    EXPECT_EQ(resources->reserved, 0u);
    broker.ReapExpiredTransactions(std::chrono::steady_clock::now());
    if (std::string(kind) == "directory") {
      EXPECT_EQ(fs::file_size(output / "marker"), 4u);
    } else if (std::string(kind) == "file") {
      EXPECT_EQ(fs::file_size(output), 4u);
    } else {
      EXPECT_EQ(fs::read_symlink(output), std::string(kind) == "live" ? source_ : storage_ / "missing");
    }
    EXPECT_EQ(fs::file_size(source_ / "image"), 5u);
    EXPECT_FALSE(fs::exists(storage_ / "missing"));
    fs::remove_all(output);
  }
}

TEST_F(BrokerTransferTest, RestoreMkdirRejectsExpiryAndCancellationWithoutLeakingStaging)
{
  for (const bool expire : {false, true}) {
    SCOPED_TRACE(expire);
    Broker broker(staging_, storage_);
    SetLifetime(broker, std::chrono::seconds(1));
    const auto resources = Limit(broker, 100);
    const auto output = staging_ / "restore/restore";
    ScopedMkdirHook hook([&](const char* path) {
      if (Path(path) != output)
        return;
      if (expire)
        std::this_thread::sleep_until(Find(broker, "restore")->cancellation().Deadline());
      else
        broker.Shutdown();
    });
    const auto response = broker.HandleRequest(Restore());
    EXPECT_EQ(response.failure().code(), expire ? Failure::TRANSACTION_EXPIRED : Failure::INTERNAL_ERROR)
        << response.DebugString();
    EXPECT_FALSE(fs::exists(output));
    EXPECT_EQ(resources->active, 0u);
    EXPECT_EQ(resources->reserved, 0u);
    EXPECT_EQ(fs::file_size(source_ / "image"), 5u);
  }
}
}  // namespace snapshot::pagebroker

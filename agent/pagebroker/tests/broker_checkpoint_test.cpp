// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "tests/broker_transfer_fixture.hpp"

#include <chrono>
#include <filesystem>
#include <fstream>
#include <future>
#include <stop_token>
#include <string>
#include <thread>
#include <vector>

namespace snapshot::pagebroker {
using test::Boundary;
using test::Configure;
using test::ScopedMkdirHook;

TEST_F(BrokerTransferTest, BothCheckpointModesUseGenericPreparationAndPublication)
{
  for (const bool direct : {false, true}) {
    SCOPED_TRACE(direct);
    Broker broker(staging_, storage_);
    std::vector<Boundary> calls;
    Intercept(broker, [&](Boundary boundary, Cancellation) { calls.push_back(boundary); });
    auto request = RequestFor(direct ? "direct-save" : "staged-save");
    auto* destination = direct ? request.mutable_prepare_direct_checkpoint()->mutable_destination()
                               : request.mutable_prepare_staged_checkpoint()->mutable_destination();
    auto* engine = direct ? request.mutable_prepare_direct_checkpoint()->mutable_io_engine()
                          : request.mutable_prepare_staged_checkpoint()->mutable_io_engine();
    Configure(destination, engine, storage_ / "saved");
    const auto prepared = broker.HandleRequest(request);
    ASSERT_FALSE(prepared.has_failure()) << prepared.DebugString();
    const auto output = direct ? prepared.direct_checkpoint_directory().image_directory()
                               : prepared.staged_checkpoint_directory().image_directory();
    std::ofstream(Path(output) / "payload") << "checkpoint";
    request.mutable_commit();
    ASSERT_TRUE(broker.HandleRequest(request).has_commit_complete());
    EXPECT_EQ(calls, (std::vector<Boundary>{Boundary::ValidateCheckpoint, Boundary::InspectCheckpoint,
                                         Boundary::PublishCheckpoint}));
    EXPECT_TRUE(fs::exists(storage_ / "saved/payload"));
    EXPECT_FALSE(fs::exists(output));
  }
}

TEST_F(BrokerTransferTest, CheckpointReservesInitialByteBeforeBackendPreparation)
{
  Broker broker(staging_, storage_);
  SetLifetime(broker, std::chrono::minutes(1));
  const auto resources = Limit(broker, 1, 2);
  unsigned preparations = 0;
  Intercept(broker, [&](Boundary boundary, Cancellation) {
    if (boundary != Boundary::ValidateCheckpoint)
      return;
    ++preparations;
    EXPECT_EQ(resources->active, 1u);
    EXPECT_EQ(resources->reserved, 1u);
  });
  ASSERT_TRUE(broker.HandleRequest(Checkpoint(false)).has_staged_checkpoint_directory());
  auto next = Checkpoint(false);
  next.set_transaction_id("next");
  EXPECT_EQ(broker.HandleRequest(next).failure().code(), Failure::INSUFFICIENT_STORAGE);
  EXPECT_EQ(preparations, 1u);
  EXPECT_EQ(resources->active, 1u);
  EXPECT_EQ(resources->reserved, 1u);
  EXPECT_FALSE(fs::exists(staging_ / "checkpoint/next"));

  auto abort = RequestFor("checkpoint");
  abort.mutable_abort();
  ASSERT_TRUE(broker.HandleRequest(abort).has_abort_complete());
  // Use a fresh ID because the rejected transaction retains its failure result.
  next.set_transaction_id("retry");
  ASSERT_TRUE(broker.HandleRequest(next).has_staged_checkpoint_directory());
  EXPECT_EQ(preparations, 2u);
}

TEST_F(BrokerTransferTest, StagedCheckpointRecreatesMissingStagingRoot)
{
  for (const bool limited : {false, true}) {
    SCOPED_TRACE(limited);
    Broker broker(staging_, storage_);
    const auto resources = limited ? Limit(broker) : nullptr;
    fs::remove_all(staging_);

    const auto response = broker.HandleRequest(Checkpoint(false));
    ASSERT_TRUE(response.has_staged_checkpoint_directory()) << response.DebugString();
    EXPECT_TRUE(fs::is_directory(CheckpointOutput(false)));
    if (resources) {
      EXPECT_EQ(resources->active, 1u);
      EXPECT_EQ(resources->reserved, 1u);
    }

    auto abort = RequestFor("checkpoint");
    abort.mutable_abort();
    ASSERT_TRUE(broker.HandleRequest(abort).has_abort_complete());
    EXPECT_FALSE(fs::exists(CheckpointOutput(false)));
    if (resources) {
      EXPECT_EQ(resources->active, 0u);
      EXPECT_EQ(resources->reserved, 0u);
    }
  }
}

TEST_F(BrokerTransferTest, CheckpointPreparationUsesTheTransactionDeadline)
{
  for (const bool direct : {false, true}) {
    SCOPED_TRACE(direct);
    Broker broker(staging_, storage_);
    SetLifetime(broker, std::chrono::seconds(1));
    Intercept(broker, [&](Boundary boundary, Cancellation control) {
      if (boundary != Boundary::ValidateCheckpoint)
        return;
      EXPECT_NE(Find(broker, "checkpoint"), nullptr);
      EXPECT_TRUE(control.Token().stop_possible());
      EXPECT_NE(control.Deadline(), Cancellation::Clock::time_point::max());
      if (control.Deadline() != Cancellation::Clock::time_point::max())
        std::this_thread::sleep_until(control.Deadline());
    });
    EXPECT_EQ(broker.HandleRequest(Checkpoint(direct)).failure().code(), Failure::TRANSACTION_EXPIRED);
    EXPECT_FALSE(fs::exists(CheckpointOutput(direct)));
  }
}

TEST_F(BrokerTransferTest, CheckpointPreparationObservesAbortAndShutdown)
{
  for (const bool direct : {false, true}) {
    for (const bool shutdown : {false, true}) {
      SCOPED_TRACE(direct);
      SCOPED_TRACE(shutdown);
      Broker broker(staging_, storage_);
      std::promise<void> preparing, resume, cancelled;
      auto resumed = resume.get_future();
      auto stopped = cancelled.get_future();
      std::stop_token token;
      Intercept(broker, [&](Boundary boundary, Cancellation control) {
        if (boundary == Boundary::ValidateCheckpoint) {
          token = control.Token();
          preparing.set_value();
          resumed.wait();
        }
      });
      const auto prepare_request = Checkpoint(direct);
      auto abort_request = RequestFor("checkpoint");
      abort_request.mutable_abort();
      auto prepare = std::async(std::launch::async, [&] { return broker.HandleRequest(prepare_request); });
      preparing.get_future().wait();
      std::stop_callback on_stop(token, [&] { cancelled.set_value(); });
      auto cancel = std::async(std::launch::async, [&] {
        if (shutdown) {
          broker.Shutdown();
          return Response{};
        }
        return broker.HandleRequest(abort_request);
      });
      EXPECT_EQ(stopped.wait_for(std::chrono::seconds(1)), std::future_status::ready);
      // Release the worker even when cancellation was not propagated.
      resume.set_value();
      EXPECT_EQ(prepare.get().failure().code(), Failure::INTERNAL_ERROR);
      const auto result = cancel.get();
      if (!shutdown) {
        EXPECT_TRUE(result.has_abort_complete()) << result.DebugString();
      }
      EXPECT_FALSE(fs::exists(CheckpointOutput(direct)));
    }
  }
}

TEST_F(BrokerTransferTest, CheckpointMkdirRejectsExpiryAndCancellationBeforeReturningReady)
{
  for (const bool direct : {false, true}) {
    for (const bool expire : {false, true}) {
      SCOPED_TRACE(direct);
      SCOPED_TRACE(expire);
      Broker broker(staging_, storage_);
      SetLifetime(broker, std::chrono::seconds(1));
      const auto output = CheckpointOutput(direct);
      ScopedMkdirHook hook([&](const char* path) {
        if (Path(path) != output)
          return;
        if (expire)
          std::this_thread::sleep_until(Find(broker, "checkpoint")->cancellation().Deadline());
        else
          broker.Shutdown();
      });
      const auto response = broker.HandleRequest(Checkpoint(direct));
      EXPECT_EQ(response.failure().code(), expire ? Failure::TRANSACTION_EXPIRED : Failure::INTERNAL_ERROR)
          << response.DebugString();
      EXPECT_FALSE(fs::exists(output));
    }
  }
}

TEST_F(BrokerTransferTest, CheckpointInterruptedCleanupRemainsRetryable)
{
  for (const bool direct : {false, true}) {
    SCOPED_TRACE(direct);
    Broker broker(staging_, storage_);
    SetLifetime(broker, std::chrono::minutes(1));
    const auto resources = Limit(broker);
    const auto output = CheckpointOutput(direct);
    const auto parent = output.parent_path();
    const auto held = root_.path() / "held-checkpoint";
    ScopedMkdirHook hook([&](const char* path) {
      if (Path(path) != output)
        return;
      Find(broker, "checkpoint")->CancelWork();
      fs::rename(parent, held);
      std::ofstream(parent) << "blocks cleanup";
    });
    const auto response = broker.HandleRequest(Checkpoint(direct));
    ASSERT_TRUE(response.has_failure()) << response.DebugString();
    EXPECT_EQ(response.failure().code(), Failure::INTERNAL_ERROR);
    EXPECT_TRUE(response.failure().message().starts_with("operation cancelled; cleanup: "))
        << response.DebugString();
    const auto transaction = Find(broker, "checkpoint");
    ASSERT_NE(transaction, nullptr);
    EXPECT_FALSE(transaction->retain_terminal());
    EXPECT_EQ(resources->active, direct ? 0u : 1u);
    EXPECT_TRUE(fs::exists(held / output.filename()));
    fs::remove(parent);
    fs::rename(held, parent);
    broker.ReapExpiredTransactions(std::chrono::steady_clock::now());
    EXPECT_FALSE(fs::exists(output));
    EXPECT_EQ(resources->active, 0u);
    EXPECT_EQ(resources->reserved, 0u);
    auto abort = RequestFor("checkpoint");
    abort.mutable_abort();
    EXPECT_TRUE(broker.HandleRequest(abort).has_abort_complete());
  }
}

TEST_F(BrokerTransferTest, CheckpointRejectionsPreserveUnownedOutputAndAllowCorrection)
{
  for (const bool direct : {false, true}) {
    SCOPED_TRACE(direct);
    Broker broker(staging_, storage_);
    SetLifetime(broker, std::chrono::minutes(1));
    const auto resources = Limit(broker);
    auto request = Checkpoint(direct);
    auto* destination = direct ? request.mutable_prepare_direct_checkpoint()->mutable_destination()
                               : request.mutable_prepare_staged_checkpoint()->mutable_destination();
    destination->mutable_filesystem()->set_directory("relative");
    EXPECT_EQ(broker.HandleRequest(request).failure().code(), Failure::INVALID_REQUEST);
    EXPECT_EQ(Find(broker, "checkpoint"), nullptr);
    EXPECT_EQ(resources->active, 0u);
    request = Checkpoint(direct);
    const auto output = CheckpointOutput(direct);
    fs::create_directory(output);
    std::ofstream(output / "existing") << "preserve";
    EXPECT_EQ(broker.HandleRequest(request).failure().code(), Failure::TRANSACTION_CONFLICT);
    EXPECT_EQ(resources->active, 0u);
    auto abort = RequestFor("checkpoint");
    abort.mutable_abort();
    EXPECT_EQ(broker.HandleRequest(abort).failure().code(), Failure::TRANSACTION_NOT_FOUND);
    EXPECT_TRUE(fs::exists(output / "existing"));
    fs::remove_all(output);
    EXPECT_FALSE(broker.HandleRequest(request).has_failure());
    EXPECT_TRUE(broker.HandleRequest(abort).has_abort_complete());
    EXPECT_EQ(resources->active, 0u);
  }
}

TEST_F(BrokerTransferTest, StagedCheckpointRetryPreservesCaptureAndAdmission)
{
  Broker broker(staging_, storage_);
  const auto resources = Limit(broker);
  const auto request = Checkpoint(false);
  ASSERT_TRUE(broker.HandleRequest(request).has_staged_checkpoint_directory());
  const auto transaction = Find(broker, "checkpoint");
  ASSERT_NE(transaction, nullptr);
  const auto deadline = transaction->cancellation().Deadline();
  const auto output = CheckpointOutput(false);
  std::ofstream(output / "payload") << "capture";
  ASSERT_EQ(resources->active, 1u);
  ASSERT_EQ(resources->reserved, 1u);

  const auto retry = Checkpoint(false);
  ASSERT_NE(retry.request_id(), request.request_id());
  EXPECT_EQ(broker.HandleRequest(retry).failure().code(), Failure::TRANSACTION_CONFLICT);
  EXPECT_EQ(Find(broker, "checkpoint"), transaction);
  EXPECT_EQ(transaction->state(), Transaction::State::STAGED);
  EXPECT_EQ(transaction->cancellation().Deadline(), deadline);
  std::string payload;
  std::ifstream(output / "payload") >> payload;
  EXPECT_EQ(payload, "capture");
  EXPECT_EQ(resources->active, 1u);
  EXPECT_EQ(resources->reserved, 1u);

  auto abort = RequestFor("checkpoint");
  abort.mutable_abort();
  ASSERT_TRUE(broker.HandleRequest(abort).has_abort_complete());
  EXPECT_FALSE(fs::exists(output));
  EXPECT_EQ(resources->active, 0u);
  EXPECT_EQ(resources->reserved, 0u);
}

TEST_F(BrokerTransferTest, CheckpointRetryRejectsModeChangeWithoutChangingCapture)
{
  for (const bool direct : {false, true}) {
    SCOPED_TRACE(direct ? "direct to staged" : "staged to direct");
    Broker broker(staging_, storage_);
    const auto resources = Limit(broker);
    const auto request = Checkpoint(direct);
    const auto prepared = broker.HandleRequest(request);
    ASSERT_TRUE(direct ? prepared.has_direct_checkpoint_directory() : prepared.has_staged_checkpoint_directory())
        << prepared.DebugString();
    const auto transaction = Find(broker, "checkpoint");
    ASSERT_NE(transaction, nullptr);
    const auto deadline = transaction->cancellation().Deadline();
    const auto output = CheckpointOutput(direct);
    std::ofstream(output / "payload") << "capture";
    const auto expected_reservation = direct ? 0u : 1u;
    ASSERT_EQ(resources->active, expected_reservation);
    ASSERT_EQ(resources->reserved, expected_reservation);

    const auto retry = Checkpoint(!direct);
    ASSERT_NE(retry.request_id(), request.request_id());
    EXPECT_EQ(broker.HandleRequest(retry).failure().code(), Failure::TRANSACTION_CONFLICT);
    EXPECT_EQ(Find(broker, "checkpoint"), transaction);
    EXPECT_EQ(transaction->state(), Transaction::State::STAGED);
    EXPECT_EQ(transaction->cancellation().Deadline(), deadline);
    std::string payload;
    std::ifstream(output / "payload") >> payload;
    EXPECT_EQ(payload, "capture");
    EXPECT_FALSE(fs::exists(CheckpointOutput(!direct)));
    EXPECT_EQ(resources->active, expected_reservation);
    EXPECT_EQ(resources->reserved, expected_reservation);

    auto abort = RequestFor("checkpoint");
    abort.mutable_abort();
    ASSERT_TRUE(broker.HandleRequest(abort).has_abort_complete());
    EXPECT_FALSE(fs::exists(output));
    EXPECT_EQ(resources->active, 0u);
    EXPECT_EQ(resources->reserved, 0u);
  }
}

TEST_F(BrokerTransferTest, DirectCheckpointRetryRejectsUnavailableEngineWithoutChangingCapture)
{
  Broker broker(staging_, storage_);
  auto request = Checkpoint(true);
  const auto prepared = broker.HandleRequest(request);
  ASSERT_TRUE(prepared.has_direct_checkpoint_directory()) << prepared.DebugString();
  const auto transaction = Find(broker, "checkpoint");
  ASSERT_NE(transaction, nullptr);
  const auto deadline = transaction->cancellation().Deadline();
  const auto output = CheckpointOutput(true);
  std::ofstream(output / "payload") << "capture";

  auto changed = Checkpoint(true);
  changed.mutable_prepare_direct_checkpoint()->mutable_io_engine()->mutable_model_streamer();
  ASSERT_NE(changed.request_id(), request.request_id());
  EXPECT_EQ(broker.HandleRequest(changed).failure().code(), Failure::INVALID_REQUEST);
  EXPECT_EQ(Find(broker, "checkpoint"), transaction);
  EXPECT_EQ(transaction->state(), Transaction::State::STAGED);
  EXPECT_EQ(transaction->cancellation().Deadline(), deadline);
  std::string payload;
  std::ifstream(output / "payload") >> payload;
  EXPECT_EQ(payload, "capture");

  // The original engine remains usable after the rejected change.
  const auto retry = broker.HandleRequest(request);
  ASSERT_TRUE(retry.has_direct_checkpoint_directory()) << retry.DebugString();
  EXPECT_EQ(retry.direct_checkpoint_directory().image_directory(), output.string());
  EXPECT_EQ(transaction->cancellation().Deadline(), deadline);
  auto abort = RequestFor("checkpoint");
  abort.mutable_abort();
  ASSERT_TRUE(broker.HandleRequest(abort).has_abort_complete());
  EXPECT_FALSE(fs::exists(output));
}

TEST_F(BrokerTransferTest, DirectCheckpointRetriesPreserveDeadlineAndCapture)
{
  for (const bool expire : {false, true}) {
    SCOPED_TRACE(expire ? "expired" : "cancelled");
    Broker broker(staging_, storage_);
    SetLifetime(broker, std::chrono::seconds(1));
    const auto request = Checkpoint(true);
    ASSERT_TRUE(broker.HandleRequest(request).has_direct_checkpoint_directory());
    const auto transaction = Find(broker, "checkpoint");
    const auto deadline = transaction->cancellation().Deadline();
    const auto output = CheckpointOutput(true);
    std::ofstream(output / "payload") << "capture";
    // Ordinary retry behavior is covered in daemon_test.cpp; both successful
    // and rejected retries must also preserve the original deadline.
    ASSERT_TRUE(broker.HandleRequest(request).has_direct_checkpoint_directory());
    EXPECT_EQ(transaction->cancellation().Deadline(), deadline);
    auto changed = request;
    changed.mutable_prepare_direct_checkpoint()->mutable_destination()->mutable_filesystem()->set_directory(
        (storage_ / "other").string());
    ASSERT_TRUE(broker.HandleRequest(changed).has_failure());
    EXPECT_EQ(transaction->cancellation().Deadline(), deadline);
    if (expire)
      std::this_thread::sleep_until(deadline);
    else
      transaction->CancelWork();
    EXPECT_EQ(broker.HandleRequest(request).failure().code(),
        expire ? Failure::TRANSACTION_EXPIRED : Failure::INTERNAL_ERROR);
    EXPECT_EQ(transaction->cancellation().Deadline(), deadline);
    EXPECT_TRUE(fs::exists(output / "payload"));
    broker.ReapExpiredTransactions(deadline);
    EXPECT_FALSE(fs::exists(output));
  }
}
}  // namespace snapshot::pagebroker

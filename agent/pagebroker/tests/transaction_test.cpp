// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "transaction.hpp"

#include <gtest/gtest.h>

#include <fstream>
#include <limits>

#include "tests/temporary_directory.hpp"

namespace snapshot::pagebroker {
namespace fs = std::filesystem;
namespace {
void
ExpectDestinationConflict(const std::shared_ptr<TransactionResources>& resources, const PublishedArtifact& artifact)
{
  Transaction contender;
  try {
    contender.PrepareTransfer(resources, 1, {}, &artifact);
    FAIL() << "destination reservation was released too early";
  }
  catch (const TransferError& error) {
    EXPECT_EQ(error.code, Failure::TRANSACTION_CONFLICT);
  }
  // A failed contender must not release the successful owner's reservation.
  contender.set_state(Transaction::State::ABORTED);
  contender.CleanupTransfer();
}
}  // namespace

TEST(TransactionTest, InitialReservationRespectsRemainingBudgetWithoutClaimingDestinationOnFailure)
{
  auto resources = std::make_shared<TransactionResources>(4, 3, std::chrono::minutes(1));
  Transaction first;
  first.PrepareTransfer(resources, 3, {});
  EXPECT_EQ(first.publication(), nullptr);
  PublishedArtifact artifact;
  artifact.set_artifact_handle(std::string(64, 'a'));
  for (const uintmax_t initial_bytes : {uintmax_t{2}, std::numeric_limits<uintmax_t>::max()}) {
    SCOPED_TRACE(initial_bytes);
    Transaction rejected;
    try {
      rejected.PrepareTransfer(resources, initial_bytes, {}, &artifact);
      FAIL() << "initial reservation exceeded the available budget";
    }
    catch (const TransferError& error) {
      EXPECT_EQ(error.code, Failure::INSUFFICIENT_STORAGE);
    }
    rejected.set_state(Transaction::State::ABORTED);
    rejected.CleanupTransfer();
  }
  EXPECT_EQ(resources->active, 1u);
  EXPECT_EQ(resources->reserved, 3u);
  EXPECT_TRUE(resources->destinations.empty());

  Transaction exact_fit;
  ASSERT_NO_THROW(exact_fit.PrepareTransfer(resources, 1, {}, &artifact));
  EXPECT_EQ(resources->active, 2u);
  EXPECT_EQ(resources->reserved, 4u);
  EXPECT_EQ(resources->destinations.count(artifact.artifact_handle()), 1u);
  // A full byte budget still rejects zero-byte admission, as before.
  Transaction blocked;
  EXPECT_THROW(blocked.PrepareTransfer(resources, 0, {}), TransferError);
  first.CleanupTransfer();
  EXPECT_EQ(resources->active, 1u);
  EXPECT_EQ(resources->reserved, 1u);
}

TEST(TransactionTest, ZeroInitialReservationCanClaimPublicationAndReleasesOnlyItsOwnResources)
{
  auto resources = std::make_shared<TransactionResources>(4, 2, std::chrono::minutes(1));
  PublishedArtifact artifact;
  artifact.set_artifact_handle(std::string(64, 'a'));
  Transaction next;
  {
    Transaction transaction;
    transaction.PrepareTransfer(resources, 0, {}, &artifact);
    ASSERT_NE(transaction.publication(), nullptr);
    EXPECT_EQ(resources->active, 1u);
    EXPECT_EQ(resources->reserved, 0u);
    transaction.Reserve(4);
    transaction.set_state(Transaction::State::ABORTED);
    transaction.CleanupTransfer();
    EXPECT_EQ(resources->active, 0u);
    EXPECT_EQ(resources->reserved, 0u);
    EXPECT_TRUE(resources->destinations.empty());
    ASSERT_NO_THROW(next.PrepareTransfer(resources, 1, {}, &artifact));
    transaction.CleanupTransfer();
  }
  EXPECT_EQ(resources->active, 1u);
  EXPECT_EQ(resources->reserved, 1u);
  EXPECT_EQ(resources->destinations.count(artifact.artifact_handle()), 1u);
}

TEST(TransactionTest, RestoreKeepsStagingBudgetUntilConsumerCleanup)
{
  test::TemporaryDirectory root;
  auto resources = std::make_shared<TransactionResources>(100, 2, std::chrono::minutes(1));
  Transaction restore;
  const auto staged = root.path() / "restored";
  restore.PrepareTransfer(resources, 0, {});
  restore.Reserve(80);
  fs::create_directory(staged);
  restore.OwnDirectory(staged);
  restore.set_state(Transaction::State::STAGED);
  EXPECT_EQ(restore.publication(), nullptr);

  Transaction next;
  next.PrepareTransfer(resources, 0, root.path() / "next");
  EXPECT_THROW(next.Reserve(21), TransferError);
  restore.RemoveStaging(staged);
  EXPECT_FALSE(fs::exists(staged));
  fs::create_directory(staged);
  std::ofstream(staged / "new-owner") << "keep";
  restore.RemoveStaging(staged); // Cleanup retries cannot release the reservation twice.
  EXPECT_NO_THROW(next.Reserve(100));
  EXPECT_EQ(resources->active, 1u);
  EXPECT_EQ(resources->reserved, 100u);
  EXPECT_EQ(fs::file_size(staged / "new-owner"), 4u);
}

TEST(TransactionTest, UnownedCleanupPreservesDirectoriesAndPendingState)
{
  for (bool limited : {false, true}) {
    SCOPED_TRACE(limited);
    test::TemporaryDirectory root;
    auto resources = std::make_shared<TransactionResources>(100, 1, std::chrono::minutes(1));
    Transaction restore;
    const auto owned = root.path() / "owned";
    const auto borrowed = root.path() / "borrowed";
    restore.PrepareTransfer(limited ? resources : nullptr, 0, {});
    fs::create_directory(owned);
    fs::create_directory(borrowed);
    std::ofstream(owned / "marker") << "owned";
    std::ofstream(borrowed / "marker") << "borrowed";
    restore.OwnDirectory(owned);
    restore.Reserve(80);
    restore.set_descriptor(RestoreTransactionDescriptor(owned));
    restore.set_state(Transaction::State::COMMITTED);

    restore.RemoveStaging(borrowed);
    std::error_code error = std::make_error_code(std::errc::permission_denied);
    restore.RemoveStaging({}, error);
    EXPECT_FALSE(error);
    EXPECT_EQ(fs::file_size(owned / "marker"), 5u);
    EXPECT_EQ(fs::file_size(borrowed / "marker"), 8u);
    restore.clear_descriptor();
    EXPECT_TRUE(std::holds_alternative<RestoreTransactionDescriptor>(restore.descriptor()));
    EXPECT_FALSE(restore.retain_terminal());
    if (limited) {
      EXPECT_EQ(resources->active, 1u);
      EXPECT_EQ(resources->reserved, 80u);
    }

    restore.CleanupTransfer();
    EXPECT_FALSE(fs::exists(owned));
    EXPECT_TRUE(fs::exists(borrowed / "marker"));
    EXPECT_TRUE(std::holds_alternative<std::monostate>(restore.descriptor()));
    EXPECT_TRUE(restore.retain_terminal());
    if (limited) {
      EXPECT_EQ(resources->active, 0u);
      EXPECT_EQ(resources->reserved, 0u);
    }
  }
}

TEST(TransactionTest, EmptyOwnedDirectoryReleasesReservation)
{
  for (bool with_error : {false, true}) {
    SCOPED_TRACE(with_error);
    auto resources = std::make_shared<TransactionResources>(100, 1, std::chrono::minutes(1));
    Transaction restore;
    restore.PrepareTransfer(resources, 0, {});
    restore.Reserve(80);
    restore.set_descriptor(RestoreTransactionDescriptor({}));
    restore.set_state(Transaction::State::ABORTED);
    ASSERT_EQ(resources->active, 1u);
    ASSERT_EQ(resources->reserved, 80u);

    if (with_error) {
      std::error_code error = std::make_error_code(std::errc::permission_denied);
      restore.RemoveStaging({}, error);
      EXPECT_FALSE(error);
    } else {
      restore.RemoveStaging({});
    }
    EXPECT_EQ(resources->active, 0u);
    EXPECT_EQ(resources->reserved, 0u);
    EXPECT_TRUE(std::holds_alternative<std::monostate>(restore.descriptor()));
    EXPECT_TRUE(restore.retain_terminal());
    restore.RemoveStaging({});
    EXPECT_EQ(resources->active, 0u);
    EXPECT_EQ(resources->reserved, 0u);
  }
}

TEST(TransactionTest, FailedCleanupRetainsReservationAndDescriptor)
{
  test::TemporaryDirectory root;
  const auto parent = root.path() / "parent";
  const auto staged = parent / "staged";
  auto resources = std::make_shared<TransactionResources>(100, 2, std::chrono::minutes(1));
  Transaction restore;
  restore.PrepareTransfer(resources, 0, staged);
  restore.Reserve(80);
  restore.set_descriptor(RestoreTransactionDescriptor(staged));
  restore.set_state(Transaction::State::COMMITTED);
  std::ofstream(parent) << "blocks cleanup";
  std::error_code error;
  restore.CleanupTransfer(error);
  ASSERT_TRUE(error);
  restore.clear_descriptor();
  EXPECT_TRUE(std::holds_alternative<RestoreTransactionDescriptor>(restore.descriptor()));
  EXPECT_FALSE(restore.retain_terminal());

  Transaction next;
  next.PrepareTransfer(resources, 0, root.path() / "next");
  EXPECT_THROW(next.Reserve(21), TransferError);
  fs::remove(parent);
  fs::create_directories(staged);
  restore.CleanupTransfer(error);
  ASSERT_FALSE(error);
  EXPECT_TRUE(restore.retain_terminal());
  EXPECT_TRUE(std::holds_alternative<std::monostate>(restore.descriptor()));
  EXPECT_NO_THROW(next.Reserve(100));
}

TEST(TransactionTest, PublicationAndDestinationOutliveStaging)
{
  test::TemporaryDirectory root;
  auto resources = std::make_shared<TransactionResources>(100, 1, std::chrono::minutes(1));
  PublishedArtifact artifact;
  artifact.set_artifact_handle(std::string(64, 'a'));
  auto checkpoint = std::make_unique<Transaction>();
  const auto staged = root.path() / "checkpoint";
  checkpoint->PrepareTransfer(resources, 1, staged, &artifact);
  {
    Transaction reader;
    EXPECT_THROW(reader.PrepareTransfer(resources, 0, root.path() / "reader"), TransferError);
  }
  checkpoint->publication()->pending_index = "saved publication bytes";
  checkpoint->publication()->published = true;
  checkpoint->set_state(Transaction::State::COMMITTED);
  checkpoint->RemoveStaging(staged);
  ASSERT_NE(checkpoint->publication(), nullptr);
  EXPECT_TRUE(checkpoint->publication()->published);
  EXPECT_EQ(checkpoint->publication()->artifact.artifact_handle(), artifact.artifact_handle());
  EXPECT_TRUE(checkpoint->publication()->pending_index.empty());
  // Failed contenders must not release the original writer's destination.
  for (unsigned attempt = 0; attempt < 2; ++attempt) {
    Transaction duplicate;
    EXPECT_THROW(duplicate.PrepareTransfer(resources, 1, root.path() / "duplicate", &artifact), TransferError);
  }
  {
    Transaction reader;
    EXPECT_NO_THROW(reader.PrepareTransfer(resources, 0, root.path() / "reader"));
  }
  checkpoint.reset();
  Transaction replacement;
  EXPECT_NO_THROW(replacement.PrepareTransfer(resources, 1, staged, &artifact));
}

TEST(TransactionTest, AbortedDestinationCanBeReusedBeforeTransactionDestruction)
{
  for (const bool has_staging : {false, true}) {
    for (const bool cleanup_first : {false, true}) {
      SCOPED_TRACE(::testing::Message() << "has_staging=" << has_staging << ", cleanup_first=" << cleanup_first);
      test::TemporaryDirectory root;
      auto resources = std::make_shared<TransactionResources>(100, 4, std::chrono::minutes(1));
      PublishedArtifact artifact;
      artifact.set_artifact_handle(std::string(64, 'a'));
      auto checkpoint = std::make_unique<Transaction>();
      const Path staged = has_staging ? root.path() / "checkpoint" : Path{};
      checkpoint->PrepareTransfer(resources, 1, staged, &artifact);
      if (has_staging)
        fs::create_directory(staged);
      checkpoint->set_state(Transaction::State::ABORTING);
      if (cleanup_first)
        checkpoint->CleanupTransfer();
      else
        checkpoint->set_state(Transaction::State::ABORTED);
      ExpectDestinationConflict(resources, artifact);
      if (cleanup_first)
        checkpoint->set_state(Transaction::State::ABORTED);
      else
        checkpoint->CleanupTransfer();
      ASSERT_TRUE(checkpoint->retain_terminal());

      Transaction replacement;
      ASSERT_NO_THROW(replacement.PrepareTransfer(resources, 1, {}, &artifact));
      // Repeated cleanup and destruction of the retained result cannot erase the new owner.
      checkpoint->CleanupTransfer();
      checkpoint->set_state(Transaction::State::ABORTED);
      ExpectDestinationConflict(resources, artifact);
      checkpoint.reset();
      ExpectDestinationConflict(resources, artifact);
      EXPECT_EQ(resources->active, 1u);
      EXPECT_EQ(resources->reserved, 1u);
    }
  }
}

TEST(TransactionTest, AbortedDestinationRemainsReservedUntilCleanupSucceeds)
{
  test::TemporaryDirectory root;
  const auto parent = root.path() / "parent";
  const auto staged = parent / "checkpoint";
  auto resources = std::make_shared<TransactionResources>(100, 4, std::chrono::minutes(1));
  PublishedArtifact artifact;
  artifact.set_artifact_handle(std::string(64, 'a'));
  Transaction checkpoint;
  checkpoint.PrepareTransfer(resources, 1, staged, &artifact);
  checkpoint.set_state(Transaction::State::ABORTED);
  std::ofstream(parent) << "blocks cleanup even when running as root";
  std::error_code error;
  checkpoint.CleanupTransfer(error);
  ASSERT_TRUE(error);
  EXPECT_FALSE(checkpoint.retain_terminal());
  ExpectDestinationConflict(resources, artifact);

  fs::remove(parent);
  fs::create_directories(staged);
  checkpoint.CleanupTransfer(error);
  ASSERT_FALSE(error);
  EXPECT_FALSE(fs::exists(staged));
  EXPECT_TRUE(checkpoint.retain_terminal());
  Transaction replacement;
  EXPECT_NO_THROW(replacement.PrepareTransfer(resources, 1, {}, &artifact));
}

TEST(TransactionTest, DeadlineAndCancellationCoverTheWholeOperation)
{
  test::TemporaryDirectory root;
  auto resources = std::make_shared<TransactionResources>(100, 1, std::chrono::minutes(1));
  Transaction restore;
  restore.PrepareTransfer(resources, 0, root.path() / "restore");
  const auto control = restore.cancellation();
  restore.set_state(Transaction::State::PREPARING);
  restore.set_state(Transaction::State::STAGED);
  EXPECT_EQ(restore.cancellation().Deadline(), control.Deadline());
  EXPECT_TRUE(restore.expired(control.Deadline(), {}));
  restore.CancelWork();
  EXPECT_THROW(control.ThrowIfCancelled(), CancellationError);
}
TEST(TransactionTest, CancellationBeforePreparationIsNotLost)
{
  Transaction transaction;
  transaction.CancelWork();
  transaction.PrepareTransfer(nullptr, 0, {}, nullptr, std::chrono::minutes{1});
  EXPECT_TRUE(transaction.cancellation().Token().stop_requested());
  EXPECT_EQ(transaction.cancellation().Reason(), CancellationError::Reason::CANCELLED);
}

TEST(TransactionTest, GpuRequestAndTransactionStopIndependently)
{
  Transaction transaction;
  transaction.PrepareTransfer(nullptr, 0, {});
  Cancellation old_request, new_request;
  auto old_operation = Cancellation::Join(transaction.cancellation(), old_request);
  old_request.Cancel();
  EXPECT_TRUE(old_operation.IsCancelled());
  EXPECT_FALSE(transaction.cancellation().IsCancelled());
  auto retry = Cancellation::Join(transaction.cancellation(), new_request);
  EXPECT_FALSE(retry.IsCancelled());
  transaction.CancelWork();
  EXPECT_TRUE(retry.IsCancelled());
  EXPECT_FALSE(new_request.IsCancelled());
}

TEST(TransactionTest, LegacyDirectDeadlineStartsAtPrepareAndIsNotExtended)
{
  Transaction transaction;
  transaction.PrepareTransfer(nullptr, 1, {}, nullptr, std::chrono::seconds{17});
  const auto deadline = transaction.cancellation().Deadline();
  transaction.set_state(Transaction::State::PREPARING);
  EXPECT_FALSE(transaction.expired(deadline - std::chrono::seconds{1}, {}));
  transaction.set_state(Transaction::State::STAGED);
  EXPECT_EQ(transaction.cancellation().Deadline(), deadline);
  EXPECT_TRUE(transaction.expired(deadline, {}));
  transaction.set_state(Transaction::State::COMMITTED);
  EXPECT_FALSE(transaction.expired(deadline + std::chrono::hours{1}, {}));
}
}  // namespace snapshot::pagebroker

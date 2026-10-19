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
    contender.PrepareTransfer(resources, 1, &artifact);
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
  auto resources = std::make_shared<TransactionResources>(4, 3);
  Transaction first;
  first.PrepareTransfer(resources, 3);
  EXPECT_EQ(first.publication(), nullptr);
  PublishedArtifact artifact;
  artifact.set_artifact_handle(std::string(64, 'a'));
  for (const uintmax_t initial_bytes : {uintmax_t{2}, std::numeric_limits<uintmax_t>::max()}) {
    SCOPED_TRACE(initial_bytes);
    Transaction rejected;
    try {
      rejected.PrepareTransfer(resources, initial_bytes, &artifact);
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
  ASSERT_NO_THROW(exact_fit.PrepareTransfer(resources, 1, &artifact));
  EXPECT_EQ(resources->active, 2u);
  EXPECT_EQ(resources->reserved, 4u);
  EXPECT_EQ(resources->destinations.count(artifact.artifact_handle()), 1u);
  first.CleanupTransfer();
  EXPECT_EQ(resources->active, 1u);
  EXPECT_EQ(resources->reserved, 1u);
}

TEST(TransactionTest, FullByteBudgetRejectsAdmissionWithSlotsAvailable)
{
  auto resources = std::make_shared<TransactionResources>(4, 2);
  Transaction first;
  first.PrepareTransfer(resources, 4);
  ASSERT_LT(resources->active, resources->transaction_limit);
  Transaction blocked;
  try {
    // Zero initial bytes isolate the full-budget check from reservation growth.
    blocked.PrepareTransfer(resources, 0);
    FAIL() << "full byte budget admitted another transaction";
  }
  catch (const TransferError& error) {
    EXPECT_EQ(error.code, Failure::INSUFFICIENT_STORAGE);
  }
  EXPECT_EQ(resources->active, 1u);
  EXPECT_EQ(resources->reserved, 4u);

  first.CleanupTransfer();
  Transaction replacement;
  ASSERT_NO_THROW(replacement.PrepareTransfer(resources, 0));
  EXPECT_NO_THROW(replacement.Reserve(4));
}

TEST(TransactionTest, FullSlotBudgetRejectsAdmissionWithBytesAvailable)
{
  auto resources = std::make_shared<TransactionResources>(100, 1);
  Transaction first;
  first.PrepareTransfer(resources, 0);
  ASSERT_LT(resources->reserved, resources->byte_limit);
  Transaction blocked;
  try {
    blocked.PrepareTransfer(resources, 0);
    FAIL() << "full slot budget admitted another transaction";
  }
  catch (const TransferError& error) {
    EXPECT_EQ(error.code, Failure::INSUFFICIENT_STORAGE);
  }
  EXPECT_EQ(resources->active, 1u);
  EXPECT_EQ(resources->reserved, 0u);

  first.CleanupTransfer();
  Transaction replacement;
  EXPECT_NO_THROW(replacement.PrepareTransfer(resources, 0));
}

TEST(TransactionTest, ZeroInitialReservationCanClaimPublicationAndReleasesOnlyItsOwnResources)
{
  auto resources = std::make_shared<TransactionResources>(4, 2);
  PublishedArtifact artifact;
  artifact.set_artifact_handle(std::string(64, 'a'));
  Transaction next;
  {
    Transaction transaction;
    transaction.PrepareTransfer(resources, 0, &artifact);
    ASSERT_NE(transaction.publication(), nullptr);
    EXPECT_EQ(resources->active, 1u);
    EXPECT_EQ(resources->reserved, 0u);
    transaction.Reserve(4);
    transaction.set_state(Transaction::State::ABORTED);
    transaction.CleanupTransfer();
    EXPECT_EQ(resources->active, 0u);
    EXPECT_EQ(resources->reserved, 0u);
    EXPECT_TRUE(resources->destinations.empty());
    ASSERT_NO_THROW(next.PrepareTransfer(resources, 1, &artifact));
    transaction.CleanupTransfer();
  }
  EXPECT_EQ(resources->active, 1u);
  EXPECT_EQ(resources->reserved, 1u);
  EXPECT_EQ(resources->destinations.count(artifact.artifact_handle()), 1u);
}

TEST(TransactionTest, RestoreKeepsStagingBudgetUntilConsumerCleanup)
{
  test::TemporaryDirectory root;
  auto resources = std::make_shared<TransactionResources>(100, 2);
  Transaction restore;
  const auto staged = root.path() / "restored";
  restore.PrepareTransfer(resources, 0);
  restore.Reserve(80);
  fs::create_directory(staged);
  restore.OwnDirectory(staged);
  restore.set_state(Transaction::State::STAGED);
  EXPECT_EQ(restore.publication(), nullptr);

  Transaction next;
  next.PrepareTransfer(resources, 0);
  EXPECT_THROW(next.Reserve(21), TransferError);
  restore.RemoveStaging(staged);
  EXPECT_FALSE(fs::exists(staged));
  fs::create_directory(staged);
  std::ofstream(staged / "new-owner") << "keep";
  restore.RemoveStaging(staged); // Repeated cleanup must preserve a new owner's directory.
  EXPECT_NO_THROW(next.Reserve(100));
  EXPECT_EQ(resources->active, 1u);
  EXPECT_EQ(resources->reserved, 100u);
  EXPECT_EQ(fs::file_size(staged / "new-owner"), 4u);
}

TEST(TransactionTest, CleanupThenDestructionReleasesReservationOnce)
{
  test::TemporaryDirectory root;
  auto resources = std::make_shared<TransactionResources>(100, 2);
  {
    Transaction restore;
    restore.PrepareTransfer(resources, 0);
    restore.Reserve(80);
    const auto staged = root.path() / "restored";
    fs::create_directory(staged);
    restore.OwnDirectory(staged);
    restore.set_state(Transaction::State::STAGED);
    restore.CleanupTransfer();
    EXPECT_FALSE(fs::exists(staged));
    EXPECT_EQ(resources->active, 0u);
    EXPECT_EQ(resources->reserved, 0u);
  }
  // The destructor must not release the already-cleaned reservation again.
  EXPECT_EQ(resources->active, 0u);
  EXPECT_EQ(resources->reserved, 0u);
  Transaction first, second;
  ASSERT_NO_THROW(first.PrepareTransfer(resources, 0));
  ASSERT_NO_THROW(second.PrepareTransfer(resources, 0));
  EXPECT_NO_THROW(first.Reserve(50));
  EXPECT_NO_THROW(second.Reserve(50));
  EXPECT_EQ(resources->active, 2u);
  EXPECT_EQ(resources->reserved, 100u);
}

TEST(TransactionTest, DescriptorClearingSucceedsAfterCleanupAndIsIdempotent)
{
  for (const bool limited : {false, true}) {
    SCOPED_TRACE(limited);
    auto resources = limited ? std::make_shared<TransactionResources>(100, 1) : nullptr;
    Transaction transaction;
    transaction.PrepareTransfer(resources, 0);
    transaction.set_state(Transaction::State::STAGED);
    transaction.set_descriptor(RestoreTransactionDescriptor({}));
    if (limited) {
      EXPECT_FALSE(transaction.TryClearDescriptor());
    }
    transaction.CleanupTransfer();
    // Cleanup leaves nonterminal descriptors for the caller to clear.
    ASSERT_TRUE(std::holds_alternative<RestoreTransactionDescriptor>(transaction.descriptor()));
    EXPECT_TRUE(transaction.TryClearDescriptor());
    EXPECT_TRUE(std::holds_alternative<std::monostate>(transaction.descriptor()));
    EXPECT_TRUE(transaction.TryClearDescriptor());
    EXPECT_TRUE(std::holds_alternative<std::monostate>(transaction.descriptor()));
  }
}

TEST(TransactionTest, UnownedCleanupPreservesDirectoriesAndPendingState)
{
  for (bool limited : {false, true}) {
    SCOPED_TRACE(limited);
    test::TemporaryDirectory root;
    auto resources = std::make_shared<TransactionResources>(100, 1);
    Transaction restore;
    const auto owned = root.path() / "owned";
    const auto borrowed = root.path() / "borrowed";
    restore.PrepareTransfer(limited ? resources : nullptr, 0);
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
    EXPECT_FALSE(restore.TryClearDescriptor());
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
    EXPECT_TRUE(restore.TryClearDescriptor());
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
    auto resources = std::make_shared<TransactionResources>(100, 1);
    Transaction restore;
    restore.PrepareTransfer(resources, 0);
    restore.Reserve(80);
    restore.set_descriptor(RestoreTransactionDescriptor({}));
    restore.set_state(Transaction::State::ABORTED);
    EXPECT_FALSE(restore.TryClearDescriptor());
    EXPECT_TRUE(std::holds_alternative<RestoreTransactionDescriptor>(restore.descriptor()));
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
  auto resources = std::make_shared<TransactionResources>(100, 2);
  Transaction restore;
  restore.PrepareTransfer(resources, 0);
  fs::create_directories(staged);
  restore.OwnDirectory(staged);
  restore.Reserve(80);
  restore.set_descriptor(RestoreTransactionDescriptor(staged));
  restore.set_state(Transaction::State::COMMITTED);
  fs::remove_all(parent);
  std::ofstream(parent) << "blocks cleanup";
  std::error_code error;
  restore.CleanupTransfer(error);
  ASSERT_TRUE(error);
  EXPECT_FALSE(restore.TryClearDescriptor());
  EXPECT_TRUE(std::holds_alternative<RestoreTransactionDescriptor>(restore.descriptor()));
  EXPECT_FALSE(restore.retain_terminal());

  Transaction next;
  next.PrepareTransfer(resources, 0);
  EXPECT_THROW(next.Reserve(21), TransferError);
  fs::remove(parent);
  fs::create_directories(staged);
  restore.CleanupTransfer(error);
  ASSERT_FALSE(error);
  EXPECT_TRUE(restore.TryClearDescriptor());
  EXPECT_TRUE(restore.retain_terminal());
  EXPECT_TRUE(std::holds_alternative<std::monostate>(restore.descriptor()));
  EXPECT_NO_THROW(next.Reserve(100));
}

TEST(TransactionTest, PublicationRetryDataSurvivesFailedCleanup)
{
  test::TemporaryDirectory root;
  const auto parent = root.path() / "parent";
  const auto held = root.path() / "held";
  const auto staged = parent / "checkpoint";
  auto resources = std::make_shared<TransactionResources>(100, 1);
  PublishedArtifact artifact;
  artifact.set_artifact_handle(std::string(64, 'a'));
  Transaction checkpoint;
  checkpoint.PrepareTransfer(resources, 1, &artifact);
  fs::create_directories(staged);
  checkpoint.OwnDirectory(staged);
  const std::string index = "saved publication retry bytes";
  checkpoint.publication()->pending_index = index;
  checkpoint.publication()->published = true;
  checkpoint.set_state(Transaction::State::COMMITTED);

  checkpoint.RemoveStaging(root.path() / "unowned");
  EXPECT_EQ(checkpoint.publication()->pending_index, index);
  fs::rename(parent, held);
  std::ofstream(parent) << "blocks cleanup even when running as root";
  std::error_code error;
  checkpoint.CleanupTransfer(error);
  ASSERT_TRUE(error);
  ASSERT_NE(checkpoint.publication(), nullptr);
  EXPECT_EQ(checkpoint.publication()->pending_index, index);

  fs::remove(parent);
  fs::rename(held, parent);
  checkpoint.CleanupTransfer(error);
  ASSERT_FALSE(error);
  ASSERT_NE(checkpoint.publication(), nullptr);
  EXPECT_TRUE(checkpoint.publication()->pending_index.empty());
  EXPECT_TRUE(checkpoint.publication()->published);
  EXPECT_EQ(checkpoint.publication()->artifact.artifact_handle(), artifact.artifact_handle());
  EXPECT_FALSE(fs::exists(staged));
}

TEST(TransactionTest, CommittedPublicationSurvivesStagingCleanup)
{
  test::TemporaryDirectory root;
  auto resources = std::make_shared<TransactionResources>(100, 1);
  PublishedArtifact artifact;
  artifact.set_artifact_handle(std::string(64, 'a'));
  Transaction checkpoint;
  const auto staged = root.path() / "checkpoint";
  checkpoint.PrepareTransfer(resources, 1, &artifact);
  fs::create_directory(staged);
  checkpoint.OwnDirectory(staged);
  checkpoint.publication()->published = true;
  checkpoint.set_state(Transaction::State::COMMITTED);
  checkpoint.CleanupTransfer();
  EXPECT_FALSE(fs::exists(staged));
  ASSERT_NE(checkpoint.publication(), nullptr);
  EXPECT_TRUE(checkpoint.publication()->published);
  EXPECT_EQ(checkpoint.publication()->artifact.artifact_handle(), artifact.artifact_handle());
}

TEST(TransactionTest, CommittedDestinationRemainsReservedUntilDestruction)
{
  test::TemporaryDirectory root;
  auto resources = std::make_shared<TransactionResources>(100, 4);
  PublishedArtifact artifact;
  artifact.set_artifact_handle(std::string(64, 'a'));
  auto checkpoint = std::make_unique<Transaction>();
  checkpoint->PrepareTransfer(resources, 1, &artifact);
  const auto staged = root.path() / "checkpoint";
  fs::create_directory(staged);
  checkpoint->OwnDirectory(staged);
  checkpoint->set_state(Transaction::State::COMMITTED);
  checkpoint->CleanupTransfer();
  ASSERT_EQ(resources->active, 0u);
  ASSERT_EQ(resources->reserved, 0u);
  ExpectDestinationConflict(resources, artifact);
  checkpoint.reset();
  Transaction replacement;
  EXPECT_NO_THROW(replacement.PrepareTransfer(resources, 1, &artifact));
}

TEST(TransactionTest, FailedContendersDoNotReleaseDestination)
{
  auto resources = std::make_shared<TransactionResources>(100, 4);
  PublishedArtifact artifact;
  artifact.set_artifact_handle(std::string(64, 'a'));
  Transaction checkpoint;
  checkpoint.PrepareTransfer(resources, 1, &artifact);
  for (unsigned attempt = 0; attempt < 2; ++attempt) {
    SCOPED_TRACE(attempt);
    ExpectDestinationConflict(resources, artifact);
  }
  EXPECT_EQ(resources->destinations.count(artifact.artifact_handle()), 1u);
}

TEST(TransactionTest, AbortedDestinationCanBeReusedBeforeTransactionDestruction)
{
  for (const bool has_staging : {false, true}) {
    for (const bool cleanup_first : {false, true}) {
      SCOPED_TRACE(::testing::Message() << "has_staging=" << has_staging << ", cleanup_first=" << cleanup_first);
      test::TemporaryDirectory root;
      auto resources = std::make_shared<TransactionResources>(100, 4);
      PublishedArtifact artifact;
      artifact.set_artifact_handle(std::string(64, 'a'));
      auto checkpoint = std::make_unique<Transaction>();
      const Path staged = has_staging ? root.path() / "checkpoint" : Path{};
      checkpoint->PrepareTransfer(resources, 1, &artifact);
      if (has_staging) {
        fs::create_directory(staged);
        checkpoint->OwnDirectory(staged);
      }
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
      ASSERT_NO_THROW(replacement.PrepareTransfer(resources, 1, &artifact));
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
  auto resources = std::make_shared<TransactionResources>(100, 4);
  PublishedArtifact artifact;
  artifact.set_artifact_handle(std::string(64, 'a'));
  Transaction checkpoint;
  checkpoint.PrepareTransfer(resources, 1, &artifact);
  fs::create_directories(staged);
  checkpoint.OwnDirectory(staged);
  checkpoint.set_state(Transaction::State::ABORTED);
  fs::remove_all(parent);
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
  EXPECT_NO_THROW(replacement.PrepareTransfer(resources, 1, &artifact));
}

TEST(TransactionTest, DeadlineAndCancellationCoverTheWholeOperation)
{
  auto resources = std::make_shared<TransactionResources>(100, 1);
  Transaction restore;
  restore.PrepareTransfer(resources, 0, nullptr, std::chrono::minutes(1));
  const auto control = restore.cancellation();
  restore.set_state(Transaction::State::PREPARING);
  restore.set_state(Transaction::State::STAGED);
  EXPECT_EQ(restore.cancellation().Deadline(), control.Deadline());
  EXPECT_TRUE(restore.expired(control.Deadline()));
  restore.CancelWork();
  EXPECT_THROW(control.ThrowIfCancelled(), CancellationError);
}
TEST(TransactionTest, CancellationBeforePreparationIsNotLost)
{
  Transaction transaction;
  transaction.CancelWork();
  transaction.PrepareTransfer(nullptr, 0, nullptr, std::chrono::minutes{1});
  EXPECT_TRUE(transaction.cancellation().Token().stop_requested());
  EXPECT_EQ(transaction.cancellation().Reason(), CancellationError::Reason::CANCELLED);
}

TEST(TransactionTest, GpuRequestAndTransactionStopIndependently)
{
  Transaction transaction;
  transaction.PrepareTransfer(nullptr, 0);
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

TEST(TransactionTest, UnpreparedTransactionDoesNotExpire)
{
  Transaction transaction;
  for (const auto now : {Cancellation::Clock::now(), Cancellation::Clock::time_point::max()}) {
    EXPECT_FALSE(transaction.expired(now));
    EXPECT_FALSE(transaction.ExpireTransfer(now));
    EXPECT_EQ(transaction.state(), Transaction::State::NEW);
    EXPECT_FALSE(transaction.cancellation().Token().stop_requested());
  }
}

TEST(TransactionTest, TerminalTransactionsDoNotExpire)
{
  for (const auto state : {Transaction::State::COMMITTED, Transaction::State::ABORTED}) {
    SCOPED_TRACE(static_cast<int>(state));
    Transaction transaction;
    transaction.PrepareTransfer(nullptr, 0);
    transaction.set_state(state);
    const auto after_deadline = transaction.cancellation().Deadline() + std::chrono::hours{1};
    EXPECT_FALSE(transaction.expired(after_deadline));
    EXPECT_FALSE(transaction.ExpireTransfer(after_deadline));
    EXPECT_EQ(transaction.state(), state);
    EXPECT_FALSE(transaction.cancellation().Token().stop_requested());
  }
}

TEST(TransactionTest, ExpiryRemainsLatchedWhileAborting)
{
  Transaction transaction;
  transaction.PrepareTransfer(nullptr, 0);
  transaction.set_state(Transaction::State::STAGED);
  const auto deadline = transaction.cancellation().Deadline();
  const auto before_deadline = deadline - std::chrono::seconds{1};
  EXPECT_FALSE(transaction.ExpireTransfer(before_deadline));
  EXPECT_EQ(transaction.state(), Transaction::State::STAGED);
  EXPECT_FALSE(transaction.cancellation().Token().stop_requested());
  EXPECT_TRUE(transaction.ExpireTransfer(deadline));
  EXPECT_EQ(transaction.state(), Transaction::State::ABORTING);
  EXPECT_TRUE(transaction.cancellation().Token().stop_requested());
  // Expiry stays recorded even if a later caller supplies an earlier time.
  EXPECT_TRUE(transaction.expired(before_deadline));
  EXPECT_TRUE(transaction.ExpireTransfer(before_deadline));
  transaction.set_state(Transaction::State::ABORTED);
  EXPECT_FALSE(transaction.expired(deadline));
}

TEST(TransactionTest, DeadlineStartsAtPrepareAndIsNotExtendedWithOrWithoutLimits)
{
  for (const bool limited : {false, true}) {
    SCOPED_TRACE(limited);
    auto resources = limited ? std::make_shared<TransactionResources>(100, 1) : nullptr;
    Transaction transaction;
    const auto lifetime = std::chrono::seconds{17};
    const auto started = Cancellation::Clock::now();
    transaction.PrepareTransfer(resources, 1, nullptr, lifetime);
    const auto deadline = transaction.cancellation().Deadline();
    EXPECT_GE(deadline, started + lifetime);
    EXPECT_LE(deadline, Cancellation::Clock::now() + lifetime);
    transaction.set_state(Transaction::State::PREPARING);
    EXPECT_FALSE(transaction.expired(deadline - std::chrono::seconds{1}));
    transaction.set_state(Transaction::State::STAGED);
    EXPECT_EQ(transaction.cancellation().Deadline(), deadline);
    EXPECT_TRUE(transaction.expired(deadline));
  }
}
}  // namespace snapshot::pagebroker

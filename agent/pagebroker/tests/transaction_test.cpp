// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "transaction.hpp"

#include <gtest/gtest.h>

#include <fstream>

#include "tests/temporary_directory.hpp"

namespace snapshot::pagebroker {
namespace fs = std::filesystem;

TEST(TransactionTest, RestoreKeepsStagingBudgetUntilConsumerCleanup)
{
  test::TemporaryDirectory root;
  auto resources = std::make_shared<TransactionResources>(100, 2, std::chrono::minutes(1));
  Transaction restore;
  const auto staged = root.path() / "restored";
  restore.PrepareTransfer(resources, Transaction::Kind::RESTORE, staged);
  restore.Reserve(80);
  fs::create_directory(staged);
  restore.set_state(Transaction::State::STAGED);
  EXPECT_EQ(restore.publication(), nullptr);

  Transaction next;
  next.PrepareTransfer(resources, Transaction::Kind::RESTORE, root.path() / "next");
  EXPECT_THROW(next.Reserve(21), TransferError);
  restore.RemoveStaging(staged);
  restore.RemoveStaging(staged); // Cleanup retries cannot release the reservation twice.
  EXPECT_NO_THROW(next.Reserve(100));
  EXPECT_FALSE(fs::exists(staged));
}

TEST(TransactionTest, FailedCleanupRetainsReservationAndDescriptor)
{
  test::TemporaryDirectory root;
  const auto parent = root.path() / "parent";
  const auto staged = parent / "staged";
  auto resources = std::make_shared<TransactionResources>(100, 2, std::chrono::minutes(1));
  Transaction restore;
  restore.PrepareTransfer(resources, Transaction::Kind::RESTORE, staged);
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
  next.PrepareTransfer(resources, Transaction::Kind::RESTORE, root.path() / "next");
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
  checkpoint->PrepareTransfer(resources, Transaction::Kind::CHECKPOINT, staged, &artifact);
  {
    Transaction reader;
    EXPECT_THROW(reader.PrepareTransfer(resources, Transaction::Kind::RESTORE, root.path() / "reader"), TransferError);
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
    EXPECT_THROW(duplicate.PrepareTransfer(resources, Transaction::Kind::CHECKPOINT, root.path() / "duplicate", &artifact), TransferError);
  }
  {
    Transaction reader;
    EXPECT_NO_THROW(reader.PrepareTransfer(resources, Transaction::Kind::RESTORE, root.path() / "reader"));
  }
  checkpoint.reset();
  Transaction replacement;
  EXPECT_NO_THROW(replacement.PrepareTransfer(resources, Transaction::Kind::CHECKPOINT, staged, &artifact));
}

TEST(TransactionTest, DeadlineAndCancellationCoverTheWholeOperation)
{
  test::TemporaryDirectory root;
  auto resources = std::make_shared<TransactionResources>(100, 1, std::chrono::minutes(1));
  Transaction restore;
  restore.PrepareTransfer(resources, Transaction::Kind::RESTORE, root.path() / "restore");
  const auto control = restore.control();
  restore.set_state(Transaction::State::PREPARING);
  restore.set_state(Transaction::State::STAGED);
  EXPECT_EQ(restore.control().deadline, control.deadline);
  EXPECT_TRUE(restore.expired(control.deadline, {}));
  restore.CancelTransfer();
  EXPECT_THROW(control.Check(), TransferInterrupted);
}
}  // namespace snapshot::pagebroker

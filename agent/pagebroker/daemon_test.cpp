// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include <gtest/gtest.h>

#include <filesystem>
#include <array>
#include <algorithm>
#include <cstdlib>
#include <iterator>
#include <cstring>
#include <fcntl.h>
#include <arpa/inet.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <sys/syscall.h>
#include <sys/wait.h>
#include <sys/resource.h>
#include <span>
#include <csignal>

#include <fstream>
#include <optional>
#include <string>
#include <thread>
#include <sys/stat.h>
#include <unistd.h>

#include "broker.hpp"
#include "daemon.hpp"
#include "gpu/storage_manifest.hpp"
#include "posix_copy_engine.hpp"

namespace fs = std::filesystem;
using namespace snapshot::pagebroker;

namespace {

class RequestBuilder {
 public:
  Request RequestFor(const std::string& transaction_id)
  {
    Request request;
    request.set_request_id("request-" + transaction_id + "-" + std::to_string(++sequence_));
    request.set_transaction_id(transaction_id);
    return request;
  }

 private:
  unsigned sequence_ = 0;
};

inline void
Configure(StorageBackend* storage, IOEngine* engine, const std::filesystem::path& directory)
{
  storage->mutable_filesystem()->set_directory(directory.string());
  engine->mutable_posix_copy();
}


class BrokerTest : public ::testing::Test, public RequestBuilder {
 protected:
  void SetUp() override
  {
    root_ = fs::temp_directory_path() / "pagebroker-daemon-tests" /
            ::testing::UnitTest::GetInstance()->current_test_info()->name();
    fs::remove_all(root_);
    source_ = root_ / "storage" / "source";
    fs::create_directories(source_);
    std::ofstream(source_ / "image") << "image";
    broker_.emplace(root_ / "tmpfs", root_ / "storage");
  }

  void TearDown() override { fs::remove_all(root_); }

  Broker& broker() { return *broker_; }

  fs::path root_;
  fs::path source_;
  std::optional<Broker> broker_;
};

TEST_F(BrokerTest, StagesRestoreAndCleansUpOnCommit)
{
  auto restore = RequestFor("restore");
  Configure(
      restore.mutable_staged_restore()->mutable_source(), restore.mutable_staged_restore()->mutable_io_engine(),
      source_);
  const auto staged = broker().HandleRequest(restore);
  ASSERT_TRUE(staged.has_staged_restore_directory());
  const fs::path staging_directory(staged.staged_restore_directory().image_directory());
  EXPECT_TRUE(fs::exists(staging_directory / "image"));

  const auto conflict = broker().HandleRequest(restore);
  ASSERT_TRUE(conflict.has_failure());
  EXPECT_EQ(conflict.failure().code(), Failure::TRANSACTION_CONFLICT);

  auto commit = RequestFor("restore");
  commit.mutable_commit();
  EXPECT_TRUE(broker().HandleRequest(commit).has_commit_complete());
  EXPECT_TRUE(broker().HandleRequest(commit).has_commit_complete());
  EXPECT_FALSE(fs::exists(staging_directory));

  auto abort = RequestFor("restore");
  abort.mutable_abort();
  const auto abort_response = broker().HandleRequest(abort);
  ASSERT_TRUE(abort_response.has_failure());
  EXPECT_EQ(abort_response.failure().code(), Failure::TRANSACTION_NOT_FOUND);
}

TEST_F(BrokerTest, StagesIndependentRestoresConcurrently)
{
  auto first = RequestFor("first");
  auto second = RequestFor("second");
  Configure(
      first.mutable_staged_restore()->mutable_source(), first.mutable_staged_restore()->mutable_io_engine(), source_);
  Configure(
      second.mutable_staged_restore()->mutable_source(), second.mutable_staged_restore()->mutable_io_engine(), source_);

  Response first_response;
  Response second_response;
  std::thread first_request([&] { first_response = broker().HandleRequest(first); });
  std::thread second_request([&] { second_response = broker().HandleRequest(second); });
  first_request.join();
  second_request.join();

  ASSERT_TRUE(first_response.has_staged_restore_directory());
  ASSERT_TRUE(second_response.has_staged_restore_directory());
  EXPECT_NE(
      first_response.staged_restore_directory().image_directory(),
      second_response.staged_restore_directory().image_directory());
}

TEST_F(BrokerTest, RejectsConcurrentRestoreForSameTransaction)
{
  auto first = RequestFor("restore");
  auto second = RequestFor("restore");
  Configure(
      first.mutable_staged_restore()->mutable_source(), first.mutable_staged_restore()->mutable_io_engine(), source_);
  Configure(
      second.mutable_staged_restore()->mutable_source(), second.mutable_staged_restore()->mutable_io_engine(), source_);

  Response first_response;
  Response second_response;
  std::thread first_request([&] { first_response = broker().HandleRequest(first); });
  std::thread second_request([&] { second_response = broker().HandleRequest(second); });
  first_request.join();
  second_request.join();

  ASSERT_NE(first_response.has_staged_restore_directory(), second_response.has_staged_restore_directory());
  const auto& rejected =
      first_response.has_staged_restore_directory() ? second_response : first_response;
  EXPECT_EQ(rejected.failure().code(), Failure::TRANSACTION_CONFLICT);
}

TEST_F(BrokerTest, ReapsExpiredStagedTransactions)
{
  auto restore = RequestFor("expired");
  Configure(
      restore.mutable_staged_restore()->mutable_source(), restore.mutable_staged_restore()->mutable_io_engine(), source_);
  const auto staged = broker().HandleRequest(restore);
  ASSERT_TRUE(staged.has_staged_restore_directory());
  const fs::path staging_directory(staged.staged_restore_directory().image_directory());

  broker().ReapExpiredTransactions(std::chrono::steady_clock::now() + std::chrono::hours(2));
  EXPECT_TRUE(fs::exists(staging_directory));

  broker().ReapExpiredTransactions(std::chrono::steady_clock::now() + std::chrono::hours(2) + std::chrono::minutes(5));
  EXPECT_FALSE(fs::exists(staging_directory));

  auto commit = RequestFor("expired");
  commit.mutable_commit();
  EXPECT_EQ(broker().HandleRequest(commit).failure().code(), Failure::TRANSACTION_NOT_FOUND);

  auto retry = RequestFor("expired");
  Configure(retry.mutable_staged_restore()->mutable_source(), retry.mutable_staged_restore()->mutable_io_engine(), source_);
  EXPECT_TRUE(broker().HandleRequest(retry).has_staged_restore_directory());
}

TEST_F(BrokerTest, CleansStaleStagingOnStart)
{
  broker_.reset();
  const fs::path stale = root_ / "tmpfs" / "restore" / "stale";
  fs::create_directories(stale);
  std::ofstream(stale / "image") << "image";

  broker_.emplace(root_ / "tmpfs", root_ / "storage");
  EXPECT_FALSE(fs::exists(stale));
}

TEST_F(BrokerTest, RejectsUnsafeTransactionIDs)
{
  for (const auto& id :
       {std::string("../escape"), std::string("nested/name"), std::string("nested\\name"),
        std::string("nul\0suffix", 10)}) {
    auto abort = RequestFor(id);
    abort.mutable_abort();
    const auto response = broker().HandleRequest(abort);
    EXPECT_TRUE(response.has_failure());
    EXPECT_EQ(response.failure().code(), Failure::INVALID_REQUEST);
  }
}

TEST_F(BrokerTest, RejectsSymlinkInRestoreSource)
{
  fs::create_symlink(root_ / "storage" / "elsewhere", source_ / "link");
  auto restore = RequestFor("symlink");
  Configure(
      restore.mutable_staged_restore()->mutable_source(), restore.mutable_staged_restore()->mutable_io_engine(),
      source_);
  const auto response = broker().HandleRequest(restore);
  ASSERT_TRUE(response.has_failure());
  EXPECT_EQ(response.failure().code(), Failure::STORAGE_ERROR);
}

TEST_F(BrokerTest, InvalidRestoreDoesNotReserveTransaction)
{
  auto invalid = RequestFor("restore");
  Configure(
      invalid.mutable_staged_restore()->mutable_source(), invalid.mutable_staged_restore()->mutable_io_engine(),
      "relative");
  EXPECT_EQ(broker().HandleRequest(invalid).failure().code(), Failure::INVALID_REQUEST);

  auto restore = RequestFor("restore");
  Configure(
      restore.mutable_staged_restore()->mutable_source(), restore.mutable_staged_restore()->mutable_io_engine(),
      source_);
  EXPECT_TRUE(broker().HandleRequest(restore).has_staged_restore_directory());
}

TEST_F(BrokerTest, RejectsPathsOutsideStorageRoot)
{
  const fs::path outside = root_ / "outside";
  fs::create_directories(outside);
  std::ofstream(outside / "image") << "image";

  auto restore = RequestFor("outside-restore");
  Configure(
      restore.mutable_staged_restore()->mutable_source(), restore.mutable_staged_restore()->mutable_io_engine(), outside);
  EXPECT_EQ(broker().HandleRequest(restore).failure().code(), Failure::INVALID_REQUEST);

  auto checkpoint = RequestFor("outside-checkpoint");
  Configure(
      checkpoint.mutable_prepare_staged_checkpoint()->mutable_destination(),
      checkpoint.mutable_prepare_staged_checkpoint()->mutable_io_engine(),
      outside / "checkpoint");
  EXPECT_EQ(broker().HandleRequest(checkpoint).failure().code(), Failure::INVALID_REQUEST);

  fs::create_directory_symlink(outside, root_ / "storage" / "link");
  auto symlinked = RequestFor("symlinked-checkpoint");
  Configure(
      symlinked.mutable_prepare_staged_checkpoint()->mutable_destination(),
      symlinked.mutable_prepare_staged_checkpoint()->mutable_io_engine(),
      root_ / "storage" / "link" / "checkpoint");
  EXPECT_EQ(broker().HandleRequest(symlinked).failure().code(), Failure::INVALID_REQUEST);

  auto root_destination = RequestFor("root-destination");
  Configure(
      root_destination.mutable_prepare_staged_checkpoint()->mutable_destination(),
      root_destination.mutable_prepare_staged_checkpoint()->mutable_io_engine(), root_ / "storage");
  EXPECT_EQ(broker().HandleRequest(root_destination).failure().code(), Failure::INVALID_REQUEST);
}

TEST_F(BrokerTest, InsufficientStagingDoesNotReserveTransaction)
{
  const fs::path large = root_ / "storage" / "large";
  fs::create_directories(large);
  std::ofstream file(large / "image");
  file.seekp(1LL << 40);
  file.put('\0');
  file.close();

  auto insufficient = RequestFor("restore");
  Configure(
      insufficient.mutable_staged_restore()->mutable_source(), insufficient.mutable_staged_restore()->mutable_io_engine(),
      large);
  EXPECT_EQ(broker().HandleRequest(insufficient).failure().code(), Failure::INSUFFICIENT_STORAGE);

  auto retry = RequestFor("restore");
  Configure(
      retry.mutable_staged_restore()->mutable_source(), retry.mutable_staged_restore()->mutable_io_engine(), source_);
  EXPECT_TRUE(broker().HandleRequest(retry).has_staged_restore_directory());
}

TEST_F(BrokerTest, RejectsInvalidStagedRestore)
{
  auto restore = RequestFor("restore");
  restore.mutable_staged_restore();
  const auto response = broker().HandleRequest(restore);
  ASSERT_TRUE(response.has_failure());
  EXPECT_EQ(response.failure().code(), Failure::INVALID_REQUEST);
}

TEST_F(BrokerTest, UnknownCommitAndAbortDoNotReserveTransactions)
{
  auto commit = RequestFor("unknown-commit");
  commit.mutable_commit();
  EXPECT_EQ(broker().HandleRequest(commit).failure().code(), Failure::TRANSACTION_NOT_FOUND);

  auto restore = RequestFor("unknown-commit");
  Configure(
      restore.mutable_staged_restore()->mutable_source(), restore.mutable_staged_restore()->mutable_io_engine(),
      source_);
  EXPECT_TRUE(broker().HandleRequest(restore).has_staged_restore_directory());

  auto abort = RequestFor("unknown-abort");
  abort.mutable_abort();
  EXPECT_EQ(broker().HandleRequest(abort).failure().code(), Failure::TRANSACTION_NOT_FOUND);

  auto prepare = RequestFor("unknown-abort");
  Configure(
      prepare.mutable_prepare_staged_checkpoint()->mutable_destination(),
      prepare.mutable_prepare_staged_checkpoint()->mutable_io_engine(), root_ / "storage" / "published");
  EXPECT_TRUE(broker().HandleRequest(prepare).has_staged_checkpoint_directory());
}

TEST_F(BrokerTest, EvictsOldTerminalTransactionsButRetainsRecentCompletions)
{
  auto oldest = RequestFor("oldest");
  Configure(
      oldest.mutable_prepare_staged_checkpoint()->mutable_destination(),
      oldest.mutable_prepare_staged_checkpoint()->mutable_io_engine(), root_ / "storage" / "oldest");
  ASSERT_TRUE(broker().HandleRequest(oldest).has_staged_checkpoint_directory());
  auto oldest_commit = RequestFor("oldest");
  oldest_commit.mutable_commit();
  ASSERT_TRUE(broker().HandleRequest(oldest_commit).has_commit_complete());

  for (size_t index = 0; index < 1024; ++index) {
    const auto id = "terminal-" + std::to_string(index);
    auto prepare = RequestFor(id);
    Configure(
        prepare.mutable_prepare_staged_checkpoint()->mutable_destination(),
        prepare.mutable_prepare_staged_checkpoint()->mutable_io_engine(), root_ / "storage" / id);
    ASSERT_TRUE(broker().HandleRequest(prepare).has_staged_checkpoint_directory());
    auto commit = RequestFor(id);
    commit.mutable_commit();
    ASSERT_TRUE(broker().HandleRequest(commit).has_commit_complete());
  }

  auto recent_commit = RequestFor("terminal-1023");
  recent_commit.mutable_commit();
  EXPECT_TRUE(broker().HandleRequest(recent_commit).has_commit_complete());

  auto reuse = RequestFor("oldest");
  Configure(
      reuse.mutable_staged_restore()->mutable_source(), reuse.mutable_staged_restore()->mutable_io_engine(), source_);
  EXPECT_TRUE(broker().HandleRequest(reuse).has_staged_restore_directory());
}

TEST_F(BrokerTest, AbortsFailedCheckpointStaging)
{
  const fs::path destination = root_ / "storage" / "published";
  const fs::path staging_directory = root_ / "tmpfs" / "checkpoint" / "checkpoint";
  fs::create_symlink(root_ / "missing", staging_directory);
  auto prepare = RequestFor("checkpoint");
  Configure(
      prepare.mutable_prepare_staged_checkpoint()->mutable_destination(),
      prepare.mutable_prepare_staged_checkpoint()->mutable_io_engine(), destination);
  const auto failed = broker().HandleRequest(prepare);
  ASSERT_TRUE(failed.has_failure());
  EXPECT_EQ(failed.failure().code(), Failure::STORAGE_ERROR);

  auto abort = RequestFor("checkpoint");
  abort.mutable_abort();
  EXPECT_TRUE(broker().HandleRequest(abort).has_abort_complete());

  const auto retry = broker().HandleRequest(prepare);
  ASSERT_TRUE(retry.has_failure());
  EXPECT_EQ(retry.failure().code(), Failure::TRANSACTION_CONFLICT);
}

TEST_F(BrokerTest, PublishesCheckpoint)
{
  const fs::path published = root_ / "storage" / "published";
  auto prepare = RequestFor("checkpoint");
  Configure(
      prepare.mutable_prepare_staged_checkpoint()->mutable_destination(),
      prepare.mutable_prepare_staged_checkpoint()->mutable_io_engine(), published);
  const auto output = broker().HandleRequest(prepare);
  ASSERT_TRUE(output.has_staged_checkpoint_directory());
  const fs::path staging_directory(output.staged_checkpoint_directory().image_directory());
  std::ofstream(staging_directory / "image") << "image";
  std::ofstream(staging_directory / ".destination") << "image";

  auto commit = RequestFor("checkpoint");
  commit.mutable_commit();
  EXPECT_TRUE(broker().HandleRequest(commit).has_commit_complete());
  EXPECT_TRUE(fs::exists(published / "image"));
  EXPECT_TRUE(fs::exists(published / ".destination"));
  EXPECT_FALSE(fs::exists(staging_directory));
}

TEST_F(BrokerTest, ReplacesExistingCheckpoint)
{
  for (const bool direct : {false, true}) {
    SCOPED_TRACE(direct ? "direct" : "staged");
    const std::string id = direct ? "direct" : "staged";
    const fs::path published = root_ / "storage" / id;
    const fs::path previous = published.string() + ".pagebroker-previous";
    fs::create_directories(published);
    std::ofstream(published / "old") << "old";
    auto prepare = RequestFor(id);
    if (direct) {
      Configure(prepare.mutable_prepare_direct_checkpoint()->mutable_destination(),
                prepare.mutable_prepare_direct_checkpoint()->mutable_io_engine(), published);
    } else {
      Configure(prepare.mutable_prepare_staged_checkpoint()->mutable_destination(),
                prepare.mutable_prepare_staged_checkpoint()->mutable_io_engine(), published);
    }
    const auto output = broker().HandleRequest(prepare);
    ASSERT_FALSE(output.has_failure()) << output.DebugString();
    const fs::path directory = direct ? output.direct_checkpoint_directory().image_directory()
                                      : output.staged_checkpoint_directory().image_directory();
    std::ofstream(directory / "new") << "new";
    struct stat before{};
    ASSERT_EQ(stat((directory / "new").c_str(), &before), 0);
    auto commit = RequestFor(id);
    commit.mutable_commit();
    ASSERT_TRUE(broker().HandleRequest(commit).has_commit_complete());
    struct stat after{};
    ASSERT_EQ(stat((published / "new").c_str(), &after), 0);
    EXPECT_EQ(before.st_dev == after.st_dev && before.st_ino == after.st_ino, direct);
    EXPECT_FALSE(fs::exists(published / "old"));
    EXPECT_FALSE(fs::exists(previous));
    EXPECT_FALSE(fs::exists(directory));
    EXPECT_FALSE(fs::exists(published.string() + ".pagebroker-partial"));
  }
}

TEST_F(BrokerTest, PreservesExistingCheckpointWhenReplacementFails)
{
  const fs::path published = root_ / "storage" / "published";
  const fs::path previous = published.string() + ".pagebroker-previous";
  fs::create_directories(published);
  std::ofstream(published / "old") << "old";
  std::ofstream(previous) << "previous";

  auto prepare = RequestFor("checkpoint");
  Configure(
      prepare.mutable_prepare_staged_checkpoint()->mutable_destination(),
      prepare.mutable_prepare_staged_checkpoint()->mutable_io_engine(), published);
  const auto output = broker().HandleRequest(prepare);
  ASSERT_TRUE(output.has_staged_checkpoint_directory());
  std::ofstream(fs::path(output.staged_checkpoint_directory().image_directory()) / "new") << "new";

  auto commit = RequestFor("checkpoint");
  commit.mutable_commit();
  EXPECT_TRUE(broker().HandleRequest(commit).has_failure());
  EXPECT_TRUE(fs::exists(published / "old"));
  EXPECT_TRUE(fs::exists(previous));
}

TEST_F(BrokerTest, PreservesExistingPartialCheckpointDestination)
{
  const fs::path destination = root_ / "storage" / "blocked";
  auto prepare = RequestFor("checkpoint");
  Configure(
      prepare.mutable_prepare_staged_checkpoint()->mutable_destination(),
      prepare.mutable_prepare_staged_checkpoint()->mutable_io_engine(), destination);
  const auto output = broker().HandleRequest(prepare);
  ASSERT_TRUE(output.has_staged_checkpoint_directory());
  std::ofstream(fs::path(output.staged_checkpoint_directory().image_directory()) / "image") << "image";
  const fs::path partial = destination.string() + ".pagebroker-partial";
  std::ofstream(partial) << "keep";

  auto commit = RequestFor("checkpoint");
  commit.mutable_commit();
  const auto response = broker().HandleRequest(commit);
  ASSERT_TRUE(response.has_failure());
  EXPECT_EQ(response.failure().code(), Failure::TRANSACTION_CONFLICT);
  EXPECT_TRUE(fs::exists(partial));
  std::string partial_contents;
  std::ifstream(partial) >> partial_contents;
  EXPECT_EQ(partial_contents, "keep");
}

TEST_F(BrokerTest, AbortsRestore)
{
  auto restore = RequestFor("restore");
  Configure(
      restore.mutable_staged_restore()->mutable_source(), restore.mutable_staged_restore()->mutable_io_engine(),
      source_);
  const auto staged = broker().HandleRequest(restore);
  ASSERT_TRUE(staged.has_staged_restore_directory());
  const fs::path staging_directory(staged.staged_restore_directory().image_directory());

  auto abort = RequestFor("restore");
  abort.mutable_abort();
  EXPECT_TRUE(broker().HandleRequest(abort).has_abort_complete());
  EXPECT_TRUE(broker().HandleRequest(abort).has_abort_complete());
  EXPECT_FALSE(fs::exists(staging_directory));

  auto commit = RequestFor("restore");
  commit.mutable_commit();
  const auto commit_response = broker().HandleRequest(commit);
  ASSERT_TRUE(commit_response.has_failure());
  EXPECT_EQ(commit_response.failure().code(), Failure::TRANSACTION_NOT_FOUND);
}

TEST_F(BrokerTest, DirectRestoreRejectsInvalidSource)
{
  const auto link = root_ / "storage" / "source-link";
  fs::create_directory_symlink(source_, link);
  for (const auto& source : {root_ / "storage" / "missing", source_ / "image", link}) {
    auto request = RequestFor("invalid-direct-source");
    Configure(request.mutable_direct_restore()->mutable_source(), request.mutable_direct_restore()->mutable_io_engine(), source);
    const auto response = broker().HandleRequest(request);
    ASSERT_TRUE(response.has_failure());
    EXPECT_EQ(response.failure().code(), Failure::INVALID_REQUEST);
  }
}

TEST_F(BrokerTest, DirectRestoreDoesNotStageOrDeleteSource)
{
  auto request = RequestFor("direct");
  Configure(request.mutable_direct_restore()->mutable_source(), request.mutable_direct_restore()->mutable_io_engine(), source_);
  ASSERT_TRUE(broker().HandleRequest(request).has_direct_restore_ready());
  EXPECT_FALSE(fs::exists(root_ / "tmpfs" / "restore" / "direct"));
  auto abort = RequestFor("direct");
  abort.mutable_abort();
  EXPECT_TRUE(broker().HandleRequest(abort).has_abort_complete());
  EXPECT_TRUE(fs::exists(source_ / "image"));
}

TEST_F(BrokerTest, DirectRestoreCommitPreservesSource)
{
  auto request = RequestFor("direct-commit");
  Configure(request.mutable_direct_restore()->mutable_source(), request.mutable_direct_restore()->mutable_io_engine(), source_);
  ASSERT_TRUE(broker().HandleRequest(request).has_direct_restore_ready());
  auto commit = RequestFor("direct-commit");
  commit.mutable_commit();
  EXPECT_TRUE(broker().HandleRequest(commit).has_commit_complete());
  EXPECT_TRUE(fs::exists(source_ / "image"));
  EXPECT_FALSE(fs::exists(root_ / "tmpfs" / "restore" / "direct-commit"));
}

TEST_F(BrokerTest, DirectRestoreExpiresFromPreparationTime)
{
  auto request = RequestFor("direct-expiry");
  Configure(request.mutable_direct_restore()->mutable_source(), request.mutable_direct_restore()->mutable_io_engine(), source_);
  ASSERT_TRUE(broker().HandleRequest(request).has_direct_restore_ready());
  const auto now = std::chrono::steady_clock::now();
  broker().ReapExpiredTransactions(now + std::chrono::hours(2));
  EXPECT_EQ(broker().HandleRequest(request).failure().code(), Failure::TRANSACTION_CONFLICT);
  broker().ReapExpiredTransactions(now + std::chrono::hours(3));
  auto commit = RequestFor("direct-expiry");
  commit.mutable_commit();
  EXPECT_EQ(broker().HandleRequest(commit).failure().code(), Failure::TRANSACTION_NOT_FOUND);
  EXPECT_TRUE(fs::exists(source_ / "image"));
}

TEST_F(BrokerTest, DirectCheckpointPreparationRepeatsAndPublishesPrivateDirectory)
{
  const auto destination = root_ / "storage" / "nested" / "checkpoint";
  auto request = RequestFor("direct-save");
  Configure(request.mutable_prepare_direct_checkpoint()->mutable_destination(),
            request.mutable_prepare_direct_checkpoint()->mutable_io_engine(), destination);
  const auto reply = broker().HandleRequest(request);
  ASSERT_TRUE(reply.has_direct_checkpoint_directory());
  const fs::path directory(reply.direct_checkpoint_directory().image_directory());
  EXPECT_EQ(directory.parent_path(), destination.parent_path());
  EXPECT_FALSE(fs::exists(destination));
  std::ofstream(directory / "payload") << "GPU and CPU output";
  request.set_request_id(RequestFor("direct-save").request_id());
  const auto repeated = broker().HandleRequest(request);
  ASSERT_TRUE(repeated.has_direct_checkpoint_directory()) << repeated.DebugString();
  EXPECT_EQ(repeated.direct_checkpoint_directory().image_directory(), directory.string());
  EXPECT_EQ(repeated.request_id(), request.request_id());
  EXPECT_TRUE(fs::exists(directory / "payload"));
  auto changed = request;
  changed.mutable_prepare_direct_checkpoint()->mutable_destination()->mutable_filesystem()->set_directory(
      (root_ / "storage" / "other").string());
  const auto conflict = broker().HandleRequest(changed);
  ASSERT_TRUE(conflict.has_failure()) << conflict.DebugString();
  EXPECT_EQ(conflict.failure().code(), Failure::TRANSACTION_CONFLICT);
  struct stat mode{};
  ASSERT_EQ(stat(directory.c_str(), &mode), 0);
  EXPECT_EQ(mode.st_mode & 0777, 0700);
  auto commit = RequestFor("direct-save");
  commit.mutable_commit();
  struct stat payload_before{};
  ASSERT_EQ(stat((directory / "payload").c_str(), &payload_before), 0);
  ASSERT_TRUE(broker().HandleRequest(commit).has_commit_complete());
  struct stat payload_after{};
  ASSERT_EQ(stat((destination / "payload").c_str(), &payload_after), 0);
  EXPECT_EQ(payload_before.st_dev, payload_after.st_dev);
  EXPECT_EQ(payload_before.st_ino, payload_after.st_ino);
  std::ifstream payload(destination / "payload");
  std::string contents;
  std::getline(payload, contents);
  EXPECT_EQ(contents, "GPU and CPU output");
  EXPECT_FALSE(fs::exists(directory));
  EXPECT_FALSE(fs::exists(destination.string() + ".pagebroker-partial"));
}

TEST_F(BrokerTest, DirectCheckpointRetainsOutputOnFailedReplacementUntilAbort)
{
  const auto destination = root_ / "storage" / "checkpoint";
  const auto previous = fs::path(destination.string() + ".pagebroker-previous");
  fs::create_directory(destination);
  std::ofstream(destination / "old") << "old";
  std::ofstream(previous) << "occupied";
  auto request = RequestFor("direct-replace");
  Configure(request.mutable_prepare_direct_checkpoint()->mutable_destination(),
            request.mutable_prepare_direct_checkpoint()->mutable_io_engine(), destination);
  const auto reply = broker().HandleRequest(request);
  ASSERT_TRUE(reply.has_direct_checkpoint_directory());
  const fs::path directory(reply.direct_checkpoint_directory().image_directory());
  std::ofstream(directory / "new") << "new";
  struct stat original{};
  ASSERT_EQ(stat((directory / "new").c_str(), &original), 0);
  auto commit = RequestFor("direct-replace");
  commit.mutable_commit();
  EXPECT_EQ(broker().HandleRequest(commit).failure().code(), Failure::STORAGE_ERROR);
  EXPECT_TRUE(fs::exists(destination / "old"));
  EXPECT_TRUE(fs::exists(directory / "new"));
  EXPECT_TRUE(fs::exists(previous));
  auto abort = RequestFor("direct-replace");
  abort.mutable_abort();
  ASSERT_TRUE(broker().HandleRequest(abort).has_abort_complete());
  EXPECT_TRUE(fs::exists(destination / "old"));
  EXPECT_FALSE(fs::exists(directory));
  EXPECT_TRUE(fs::exists(previous));
}

TEST_F(BrokerTest, CpuStagingOmitsGpuPayloadsWithoutChangingTheSource)
{
  fs::create_directories(source_ / gpu::kDataDirectory / "1");
  std::ofstream(source_ / gpu::kDataDirectory / "1" / "extent") << "GPU payload";
  PosixCopyEngine engine(root_ / "storage");
  StorageBackend source;
  source.mutable_filesystem()->set_directory(source_.string());
  const auto cpu = root_ / "cpu";
  engine.StageRestore(source, cpu);
  EXPECT_TRUE(fs::exists(cpu / "image"));
  EXPECT_FALSE(fs::exists(cpu / gpu::kDataDirectory));
  EXPECT_TRUE(fs::exists(source_ / gpu::kDataDirectory / "1" / "extent"));
  EXPECT_EQ(engine.RestoreSize(source), fs::file_size(source_ / "image"));
}

TEST_F(BrokerTest, RejectsExistingDirectCheckpointOutput)
{
  const auto destination = root_ / "storage" / "checkpoint";
  const auto directory = fs::path(destination.string() + ".pagebroker-tx-existing");
  fs::create_directory(directory);
  std::ofstream(directory / "image") << "existing";
  auto request = RequestFor("existing");
  auto* prepare = request.mutable_prepare_direct_checkpoint();
  Configure(prepare->mutable_destination(), prepare->mutable_io_engine(), destination);
  const auto result = broker().HandleRequest(request);
  ASSERT_TRUE(result.has_failure());
  EXPECT_EQ(result.failure().code(), Failure::TRANSACTION_CONFLICT);
  EXPECT_EQ(result.failure().message(), "checkpoint output directory already exists");
  EXPECT_TRUE(fs::exists(directory / "image"));
}

TEST(DaemonOptionsTest, ParsesCustomStorageOptions)
{
  const std::vector<std::string_view> arguments{
      "socket", "staging", "storage", "--custom-storage-engine", "off",
      "--max-concurrent-requests", "8", "--custom-storage-buffer-count", "2",
      "--custom-storage-chunk-bytes", "1048576", "--custom-storage-max-pinned-bytes", "0"};
  const auto options = ParseDaemonOptions(arguments);
  EXPECT_EQ(options.socket_path, "socket");
  EXPECT_EQ(options.staging_directory, "staging");
  EXPECT_EQ(options.storage_root, "storage");
  EXPECT_EQ(options.max_concurrent_requests, 8);
  EXPECT_EQ(options.gpu.buffer_count, 2);
  EXPECT_EQ(options.gpu.chunk_bytes, 1048576);
  EXPECT_EQ(options.gpu.max_pinned_bytes, 0);
  EXPECT_FALSE(options.enable_gpu);
}

TEST(DaemonOptionsTest, RejectsInvalidOptions)
{
  const std::vector<std::vector<std::string_view>> invalid{
      {"--custom-storage-engine"}, {"--custom-storage-engine", "yes"},
      {"--custom-storage-buffer-count", "0"}, {"--custom-storage-chunk-bytes", "0"},
      {"--max-concurrent-requests", "0"}, {"--custom-storage-max-pinned-bytes", "-1"},
      {"--custom-storage-buffer-count", "184467440737095516160"},
      {"--custom-storage-chunk-bytes", "12bytes"}, {"--unknown", "1"}};
  for (const auto& suffix : invalid) {
    std::vector<std::string_view> arguments{"socket", "staging", "storage"};
    arguments.insert(arguments.end(), suffix.begin(), suffix.end());
    SCOPED_TRACE(suffix.front());
    try {
      ParseDaemonOptions(arguments);
      FAIL() << "invalid options were accepted";
    } catch (const std::invalid_argument& error) {
      EXPECT_NE(std::string(error.what()).find(suffix.front()), std::string::npos);
    }
  }
  EXPECT_THROW(ParseDaemonOptions({}), std::invalid_argument);
}

// Exercise the shipped listener and framing with real SCM_RIGHTS descriptors.
class DaemonTransportTest : public ::testing::Test, public RequestBuilder {
 protected:
  void SetUp() override
  {
    root_ = fs::temp_directory_path() / ("pagebroker-transport-" + std::to_string(getpid()));
    fs::remove_all(root_);
    fs::create_directories(root_ / "storage");
    socket_path_ = root_ / "control.sock";
    server_ = fork();
    ASSERT_GE(server_, 0);
    if (server_ == 0) {
      DaemonOptions options;
      options.socket_path = socket_path_;
      options.staging_directory = root_ / "staging";
      options.storage_root = root_ / "storage";
      options.enable_gpu = false;
      std::_Exit(static_cast<int>(RunDaemon(options)));
    }
    const auto deadline = std::chrono::steady_clock::now() + std::chrono::seconds{5};
    while (!fs::exists(socket_path_) && std::chrono::steady_clock::now() < deadline) {
      std::this_thread::sleep_for(std::chrono::milliseconds{10});
    }
    ASSERT_TRUE(fs::exists(socket_path_));
  }

  void TearDown() override
  {
    if (server_ > 0) {
      kill(server_, SIGTERM);
      while (waitpid(server_, nullptr, 0) < 0 && errno == EINTR) {}
    }
    fs::remove_all(root_);
  }

  FileDescriptor Connect()
  {
    FileDescriptor connection(socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0));
    sockaddr_un address{};
    address.sun_family = AF_UNIX;
    if (socket_path_.string().size() >= sizeof(address.sun_path)) {
      throw std::runtime_error("test socket path is too long");
    }
    std::strcpy(address.sun_path, socket_path_.c_str());
    if (connect(connection.get(), reinterpret_cast<sockaddr*>(&address), sizeof(address))) {
      throw std::system_error(errno, std::generic_category(), "connect test daemon");
    }
    constexpr timeval timeout{5, 0};
    if (setsockopt(connection.get(), SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout))) {
      throw std::runtime_error("set test socket timeout");
    }
    return connection;
  }

  void Send(int connection, const void* bytes, size_t size, std::span<const int> files = {})
  {
    iovec data{const_cast<void*>(bytes), size};
    alignas(cmsghdr) std::array<char, CMSG_SPACE(253 * sizeof(int))> control{};
    msghdr header{};
    header.msg_iov = &data;
    header.msg_iovlen = 1;
    if (!files.empty()) {
      if (files.size() > 253) {
        throw std::runtime_error("too many test descriptors");
      }
      header.msg_control = control.data();
      header.msg_controllen = CMSG_SPACE(files.size() * sizeof(int));
      auto* rights = CMSG_FIRSTHDR(&header);
      rights->cmsg_level = SOL_SOCKET;
      rights->cmsg_type = SCM_RIGHTS;
      rights->cmsg_len = CMSG_LEN(files.size() * sizeof(int));
      std::copy(files.begin(), files.end(), reinterpret_cast<int*>(CMSG_DATA(rights)));
    }
    if (sendmsg(connection, &header, MSG_NOSIGNAL) != static_cast<ssize_t>(size)) {
      throw std::runtime_error("send test request");
    }
  }

  Response Receive(int connection)
  {
    uint32_t size = 0;
    if (recv(connection, &size, sizeof(size), MSG_WAITALL) != static_cast<ssize_t>(sizeof(size))) {
      throw std::runtime_error("read test response size");
    }
    std::string body(ntohl(size), '\0');
    Response response;
    if (recv(connection, body.data(), body.size(), MSG_WAITALL) != static_cast<ssize_t>(body.size()) ||
        !response.ParseFromString(body)) {
      throw std::runtime_error("read test response");
    }
    char extra;
    if (recv(connection, &extra, 1, 0) != 0) {
      throw std::runtime_error("test connection did not close after response");
    }
    return response;
  }

  Response Exchange(const Request& request, std::initializer_list<int> files = {})
  {
    auto connection = Connect();
    const auto message = request.SerializeAsString();
    const uint32_t size = htonl(message.size());
    Send(connection.get(), &size, sizeof(size), {files.begin(), files.size()});
    Send(connection.get(), message.data(), message.size());
    return Receive(connection.get());
  }

  auto OpenFiles() const
  {
    return std::distance(fs::directory_iterator("/proc/" + std::to_string(server_) + "/fd"),
                         fs::directory_iterator());
  }

  bool WaitForPidfd() const
  {
    const auto deadline = std::chrono::steady_clock::now() + std::chrono::seconds{5};
    do {
      for (const auto& entry : fs::directory_iterator("/proc/" + std::to_string(server_) + "/fd")) {
        std::error_code error;
        if (fs::read_symlink(entry, error) == "anon_inode:[pidfd]") {
          return true;
        }
      }
      std::this_thread::yield();
    } while (std::chrono::steady_clock::now() < deadline);
    return false;
  }

  pid_t server_ = -1;
  fs::path root_;
  fs::path socket_path_;
};

TEST_F(DaemonTransportTest, BindsGpuDescriptorsAndPreservesControlRequests)
{
  auto ready = RequestFor("ready");
  ready.mutable_capabilities();
  ASSERT_TRUE(Exchange(ready).has_capabilities());
  const auto initial_files = OpenFiles();
  FileDescriptor process(static_cast<int>(syscall(SYS_pidfd_open, getpid(), 0)));
  FileDescriptor ordinary(open("/dev/null", O_RDONLY | O_CLOEXEC));
  ASSERT_GE(process.get(), 0);
  ASSERT_GE(ordinary.get(), 0);
  auto request = RequestFor("gpu");
  auto* checkpoint = request.mutable_checkpoint_gpu();
  checkpoint->mutable_context()->add_captured_pids(1);
  checkpoint->mutable_context()->add_visible_devices("GPU-11111111-1111-1111-1111-111111111111");
  auto* target = checkpoint->add_targets();
  target->set_captured_pid(1);
  target->set_target_pid(getpid());

  const auto valid = Exchange(request, {process.get()});
  ASSERT_TRUE(valid.has_failure()) << valid.DebugString();
  // Descriptor validation succeeded. This CPU test deliberately disables CUDA.
  EXPECT_EQ(valid.failure().message(), "CustomStorage is unavailable");
  for (const auto& invalid : {Exchange(request), Exchange(request, {process.get(), process.get()}),
                              Exchange(request, {ordinary.get()})}) {
    EXPECT_EQ(invalid.failure().code(), Failure::INVALID_REQUEST) << invalid.DebugString();
    EXPECT_NE(invalid.failure().message(), valid.failure().message());
  }
  target->set_target_pid(std::max(getpid(), server_) + 1);
  const auto mismatched = Exchange(request, {process.get()});
  EXPECT_EQ(mismatched.failure().message(), "GPU target descriptor does not match host PID");

  const auto child = fork();
  ASSERT_GE(child, 0);
  if (child == 0) {
    pause();
    std::_Exit(0);
  }
  FileDescriptor exited(static_cast<int>(syscall(SYS_pidfd_open, child, 0)));
  kill(child, SIGKILL);
  while (waitpid(child, nullptr, 0) < 0 && errno == EINTR) {}
  ASSERT_GE(exited.get(), 0);
  target->set_target_pid(child);
  const auto dead = Exchange(request, {exited.get()});
  EXPECT_EQ(dead.failure().code(), Failure::INVALID_REQUEST) << dead.DebugString();
  EXPECT_NE(dead.failure().message(), valid.failure().message());

  auto capabilities = RequestFor("control");
  capabilities.mutable_capabilities();
  const auto wrong_control = Exchange(capabilities, {process.get()});
  EXPECT_EQ(wrong_control.failure().code(), Failure::INVALID_REQUEST);
  const auto control = Exchange(capabilities);
  EXPECT_TRUE(control.has_capabilities()) << control.DebugString();
  EXPECT_EQ(OpenFiles(), initial_files) << "request descriptors leaked";
}

TEST_F(DaemonTransportTest, HandlesFragmentedHeadersAndRejectsLateOrTruncatedRights)
{
  auto ready = RequestFor("ready");
  ready.mutable_capabilities();
  ASSERT_TRUE(Exchange(ready).has_capabilities());
  const auto initial_files = OpenFiles();
  FileDescriptor process(static_cast<int>(syscall(SYS_pidfd_open, getpid(), 0)));
  ASSERT_GE(process.get(), 0);
  const std::array<int, 1> files{process.get()};
  auto request = RequestFor("fragmented");
  auto* checkpoint = request.mutable_checkpoint_gpu();
  checkpoint->mutable_context()->add_captured_pids(1);
  checkpoint->mutable_context()->add_visible_devices("GPU-11111111-1111-1111-1111-111111111111");
  auto* target = checkpoint->add_targets();
  target->set_captured_pid(1);
  target->set_target_pid(getpid());
  const auto message = request.SerializeAsString();
  const uint32_t size = htonl(message.size());
  for (const auto& late : {std::string("none"), std::string("header"), std::string("body")}) {
    SCOPED_TRACE(late);
    auto connection = Connect();
    Send(connection.get(), &size, 1, files);
    // Descriptor adoption proves that the first recvmsg consumed exactly this
    // fragment before sending the rest. No timing assumption or syscall stub.
    ASSERT_TRUE(WaitForPidfd());
    Send(connection.get(), reinterpret_cast<const char*>(&size) + 1, sizeof(size) - 1,
         late == "header" ? std::span<const int>(files) : std::span<const int>{});
    if (late == "header") {
      char byte;
      EXPECT_EQ(recv(connection.get(), &byte, 1, 0), 0);
    } else {
      Send(connection.get(), message.data(), message.size(),
           late == "body" ? std::span<const int>(files) : std::span<const int>{});
      const auto response = Receive(connection.get());
      ASSERT_TRUE(response.has_failure()) << response.DebugString();
      EXPECT_EQ(response.failure().message(), late == "none" ? "CustomStorage is unavailable" : "invalid request");
    }
    ASSERT_TRUE(Exchange(ready).has_capabilities());
    EXPECT_EQ(OpenFiles(), initial_files);
  }

  // The kernel's SCM_RIGHTS send limit equals the receiver's buffer capacity.
  // Lower the running child's descriptor limit to induce real MSG_CTRUNC.
  rlimit original{};
  ASSERT_EQ(prlimit(server_, RLIMIT_NOFILE, nullptr, &original), 0);
  rlimit limited{static_cast<rlim_t>(initial_files + 8), original.rlim_max};
  ASSERT_EQ(prlimit(server_, RLIMIT_NOFILE, &limited, nullptr), 0);
  auto connection = Connect();
  const std::array<int, 32> many = [&] {
    std::array<int, 32> result;
    result.fill(process.get());
    return result;
  }();
  Send(connection.get(), &size, sizeof(size), many);
  const auto truncated = Receive(connection.get());
  EXPECT_EQ(truncated.failure().code(), Failure::INVALID_REQUEST);
  EXPECT_EQ(truncated.failure().message(), "invalid request descriptors");
  ASSERT_EQ(prlimit(server_, RLIMIT_NOFILE, &original, nullptr), 0);
  ASSERT_TRUE(Exchange(ready).has_capabilities());
  EXPECT_EQ(OpenFiles(), initial_files);
}

TEST_F(DaemonTransportTest, RejectsGpuAdmissionFromAControlHandlerDuringShutdown)
{
  FileDescriptor process(static_cast<int>(syscall(SYS_pidfd_open, getpid(), 0)));
  ASSERT_GE(process.get(), 0);
  auto request = RequestFor("shutdown");
  request.mutable_checkpoint_gpu();
  const auto message = request.SerializeAsString();
  const uint32_t size = htonl(message.size());
  auto connection = Connect();
  const std::array<int, 1> files{process.get()};
  Send(connection.get(), &size, 1, files);
  ASSERT_TRUE(WaitForPidfd());
  ASSERT_EQ(kill(server_, SIGTERM), 0);
  // Closing the listener is observable before the blocked control handler
  // finishes its frame. The handler must not admit a new GPU operation now.
  bool stopped = false;
  const auto deadline = std::chrono::steady_clock::now() + std::chrono::seconds{5};
  while (std::chrono::steady_clock::now() < deadline) {
    try {
      auto probe = Connect();
    } catch (const std::system_error& error) {
      stopped = error.code().value() == ECONNREFUSED;
      break;
    }
    std::this_thread::yield();
  }
  EXPECT_TRUE(stopped);
  Send(connection.get(), reinterpret_cast<const char*>(&size) + 1, sizeof(size) - 1);
  Send(connection.get(), message.data(), message.size());
  const auto response = Receive(connection.get());
  EXPECT_EQ(response.failure().code(), Failure::UNAVAILABLE);
  EXPECT_EQ(response.failure().message(), "PageBroker is shutting down");
}

}  // namespace

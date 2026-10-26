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
#include <poll.h>
#include <span>
#include <csignal>
#include <chrono>
#include <condition_variable>
#include <mutex>

#include <fstream>
#include <iterator>
#include <memory>
#include <optional>
#include <string>
#include <thread>
#include <sys/stat.h>
#include <unistd.h>

#include "broker.hpp"
#include "daemon.hpp"
#include "gpu/storage_manifest.hpp"
#include "tests/request_builder.hpp"
#include "tests/temporary_directory.hpp"
#include "transfer/engine/posix/posix_copy_engine.hpp"

namespace fs = std::filesystem;
using namespace snapshot::pagebroker;

namespace {

S3Config TestS3Config()
{
  S3Config config;
  config.store_id = "test-store";
  config.bucket = "test-bucket";
  config.transfer.connection.region = "us-east-1";
  config.transfer.connection.endpoint = "https://unused.invalid";
  const auto* addressing = std::getenv(kS3VirtualAddressingEnv);
  config.transfer.connection.use_virtual_addressing = !addressing || std::string(addressing) != kS3VirtualAddressingDisabled;
  if (const auto* ca = std::getenv(kAwsCaBundleEnv))
    config.transfer.connection.ca_file = ca;
  return config;
}

}  // namespace

namespace {

using test::Configure;

class BrokerTest : public ::testing::Test, public test::RequestBuilder {
 protected:
  void SetUp() override
  {
    root_ = temporary_.path();
    source_ = root_ / "storage" / "source";
    fs::create_directories(source_);
    std::ofstream(source_ / "image") << "image";
    broker_.emplace(root_ / "tmpfs", root_ / "storage");
  }

  void TearDown() override { broker_.reset(); }

  Broker& broker() { return *broker_; }

  test::TemporaryDirectory temporary_;
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

TEST_F(BrokerTest, RejectsFilesystemModelStreamerBeforeStaging)
{
  auto request = RequestFor("unsupported");
  auto* restore = request.mutable_staged_restore();
  Configure(restore->mutable_source(), restore->mutable_io_engine(), source_);
  restore->mutable_io_engine()->mutable_model_streamer();
  const auto response = broker().HandleRequest(request);
  ASSERT_TRUE(response.has_failure());
  EXPECT_EQ(response.failure().code(), Failure::INVALID_REQUEST);
  EXPECT_TRUE(fs::is_empty(root_ / "tmpfs" / "restore"));
  EXPECT_TRUE(fs::exists(source_ / "image"));
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

TEST(BrokerStartupTest, RemovesReadonlyRestoreAndCheckpointStagingWithoutRecoveringTransactions)
{
  test::TemporaryDirectory root;
  const auto storage = root.path() / "storage";
  const auto source = storage / "published";
  const auto staging = root.path() / "staging";
  fs::create_directories(source / "nested");
  std::ofstream(source / "nested/image") << "published checkpoint";
  const auto readonly = fs::perms::owner_read | fs::perms::owner_exec;
  fs::permissions(source / "nested", readonly);
  fs::permissions(source, readonly);
  test::RequestBuilder requests;
  {
    Broker broker(staging, storage);
    auto restore = requests.RequestFor("interrupted-restore");
    Configure(restore.mutable_staged_restore()->mutable_source(),
              restore.mutable_staged_restore()->mutable_io_engine(), source);
    ASSERT_TRUE(broker.HandleRequest(restore).has_staged_restore_directory());
    auto checkpoint = requests.RequestFor("interrupted-checkpoint");
    Configure(checkpoint.mutable_prepare_staged_checkpoint()->mutable_destination(),
              checkpoint.mutable_prepare_staged_checkpoint()->mutable_io_engine(), storage / "next");
    const auto prepared = broker.HandleRequest(checkpoint);
    ASSERT_TRUE(prepared.has_staged_checkpoint_directory());
    const fs::path directory(prepared.staged_checkpoint_directory().image_directory());
    fs::create_directory(directory / "nested");
    std::ofstream(directory / "nested/image") << "unfinished checkpoint";
    fs::permissions(directory / "nested", fs::perms::none);
    fs::permissions(directory, readonly);
  }

  // Unprivileged restart must remove read-only staging and forget transactions.
  Broker restarted(staging, storage);
  EXPECT_TRUE(fs::is_empty(staging / "restore"));
  EXPECT_TRUE(fs::is_empty(staging / "checkpoint"));
  for (const auto* id : {"interrupted-restore", "interrupted-checkpoint"}) {
    auto commit = requests.RequestFor(id);
    commit.mutable_commit();
    EXPECT_EQ(restarted.HandleRequest(commit).failure().code(), Failure::TRANSACTION_NOT_FOUND);
  }
  EXPECT_EQ(fs::status(source).permissions(), readonly);
  EXPECT_EQ(fs::status(source / "nested").permissions(), readonly);
  EXPECT_EQ(fs::file_size(source / "nested/image"), 20u);
  EXPECT_FALSE(fs::exists(storage / "next"));
}

TEST(BrokerStartupTest, PreservesExternalTargetsOfStagingLinks)
{
  test::TemporaryDirectory root;
  const auto outside = root.path() / "published";
  const auto staging = root.path() / "staging";
  fs::create_directories(outside);
  fs::create_directories(outside / "restore");
  std::ofstream(outside / "restore/image") << "external staging";
  std::ofstream(outside / "image") << "published";
  fs::permissions(outside, fs::perms::owner_read | fs::perms::owner_exec);
  fs::permissions(outside / "image", fs::perms::owner_read);
  fs::create_directories(staging / "checkpoint/stale");
  fs::create_directory_symlink(outside, staging / "restore");
  fs::create_directory_symlink(outside, staging / "s3");
  fs::create_directory_symlink(outside, staging / "checkpoint/stale/directory");
  fs::create_symlink(outside / "image", staging / "checkpoint/stale/file");
  fs::create_hard_link(outside / "image", staging / "checkpoint/stale/hard-link");
  Broker restarted(staging, root.path());
  EXPECT_TRUE(fs::is_empty(staging / "restore"));
  EXPECT_TRUE(fs::is_empty(staging / "checkpoint"));
  EXPECT_FALSE(fs::is_symlink(staging / "restore"));
  EXPECT_EQ(fs::status(outside).permissions(), fs::perms::owner_read | fs::perms::owner_exec);
  EXPECT_EQ(fs::status(outside / "image").permissions(), fs::perms::owner_read);
  EXPECT_EQ(fs::file_size(outside / "image"), 9u);
  EXPECT_EQ(fs::file_size(outside / "restore/image"), 16u);
  EXPECT_FALSE(fs::is_symlink(staging / "s3"));
}

TEST(BrokerStartupTest, CleansBothNamespacesWhenSwitchingStorageModes)
{
  for (const bool use_s3 : {false, true}) {
    SCOPED_TRACE(use_s3 ? "POSIX to S3" : "S3 to POSIX");
    test::TemporaryDirectory root;
    const auto staging = root.path() / "staging";
    // Both namespaces can contain leftovers from earlier daemon instances.
    for (const auto& namespace_root : {staging, staging / "s3"}) {
      for (const auto* kind : {"restore", "checkpoint"}) {
        const auto stale = namespace_root / kind / "interrupted";
        fs::create_directories(stale / "nested");
        std::ofstream(stale / "nested/image") << "unfinished";
        fs::permissions(stale / "nested", fs::perms::none);
        fs::permissions(stale, fs::perms::owner_read | fs::perms::owner_exec);
      }
      std::ofstream(namespace_root / "unrelated") << "keep";
    }
    Broker restarted(staging, root.path() / "storage", nullptr,
                     use_s3 ? std::optional(TestS3Config()) : std::nullopt);
    const auto active = use_s3 ? staging / "s3" : staging;
    const auto inactive = use_s3 ? staging : staging / "s3";
    for (const auto* kind : {"restore", "checkpoint"}) {
      EXPECT_TRUE(fs::is_empty(active / kind));
      EXPECT_FALSE(fs::exists(inactive / kind));
    }
    EXPECT_EQ(fs::file_size(staging / "unrelated"), 4u);
    EXPECT_EQ(fs::file_size(staging / "s3/unrelated"), 4u);
  }
}

TEST(BrokerStartupTest, PreservesStagingWhenEngineConfigurationIsInvalid)
{
  test::TemporaryDirectory root;
  const auto staging = root.path() / "staging";
  for (const auto& namespace_root : {staging, staging / "s3"}) {
    fs::create_directories(namespace_root / "restore");
    std::ofstream(namespace_root / "restore/image") << "keep";
  }
  auto config = TestS3Config();
  config.transfer.restore_timeout = std::chrono::milliseconds::zero();
  EXPECT_THROW((Broker(staging, root.path(), nullptr, config)), std::invalid_argument);
  EXPECT_EQ(fs::file_size(staging / "restore/image"), 4u);
  EXPECT_EQ(fs::file_size(staging / "s3/restore/image"), 4u);
}

TEST_F(BrokerTest, RejectsUnsafeTransactionIDs)
{
  PosixCopyEngine engine(root_ / "storage");
  StorageBackend destination;
  const auto published = root_ / "storage" / "published";
  destination.mutable_filesystem()->set_directory(published.string());
  for (const auto& id :
       {std::string(), std::string("."), std::string(".."), std::string("../escape"),
        std::string("nested/name"), std::string("nested\\name"),
        std::string("nul\0suffix", 10)}) {
    SCOPED_TRACE(id);
    auto abort = RequestFor(id);
    abort.mutable_abort();
    const auto response = broker().HandleRequest(abort);
    EXPECT_TRUE(response.has_failure());
    EXPECT_EQ(response.failure().code(), Failure::INVALID_REQUEST);
    EXPECT_THROW(engine.PrepareCheckpoint(destination, {}, id, TransferMode::Direct), std::invalid_argument);
  }
  EXPECT_EQ(engine.PrepareCheckpoint(destination, {}, "valid-id", TransferMode::Direct),
            Path(published.string() + ".pagebroker-tx-valid-id"));
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
  EXPECT_EQ(response.failure().code(), Failure::INVALID_REQUEST);
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

TEST_F(BrokerTest, RejectsUnsafeRestoreSourcePaths)
{
  unsigned sequence = 0;
  for (const bool direct : {false, true}) {
    for (const auto& source : {
             source_.string() + "/", source_.string() + std::string("\0suffix", 7),
             (root_ / "storage").string() + std::string("/\0suffix", 8),
             (root_ / "storage").string(), (root_ / "outside").string()}) {
      SCOPED_TRACE(::testing::Message() << "direct=" << direct << ", source=" << ::testing::PrintToString(source));
      auto request = RequestFor("invalid-source-" + std::to_string(++sequence));
      auto* storage = direct ? request.mutable_direct_restore()->mutable_source()
                             : request.mutable_staged_restore()->mutable_source();
      auto* engine = direct ? request.mutable_direct_restore()->mutable_io_engine()
                            : request.mutable_staged_restore()->mutable_io_engine();
      Configure(storage, engine, source);
      const auto response = broker().HandleRequest(request);
      EXPECT_TRUE(response.has_failure());
      EXPECT_EQ(response.failure().code(), Failure::INVALID_REQUEST);
    }
  }
  EXPECT_TRUE(fs::exists(source_ / "image"));
}

TEST_F(BrokerTest, RejectsUnsafeCheckpointDestinationPaths)
{
  const auto destination = (root_ / "storage" / "checkpoint").string();
  unsigned sequence = 0;
  for (const bool direct : {false, true}) {
    for (const auto& path : {
             destination + "/", destination + std::string("\0suffix", 7),
             (root_ / "storage").string() + std::string("/\0suffix", 8),
             (root_ / "storage").string(), (root_ / "outside").string()}) {
      SCOPED_TRACE(::testing::Message() << "direct=" << direct << ", destination=" << ::testing::PrintToString(path));
      auto request = RequestFor("invalid-destination-" + std::to_string(++sequence));
      auto* storage = direct ? request.mutable_prepare_direct_checkpoint()->mutable_destination()
                             : request.mutable_prepare_staged_checkpoint()->mutable_destination();
      auto* engine = direct ? request.mutable_prepare_direct_checkpoint()->mutable_io_engine()
                            : request.mutable_prepare_staged_checkpoint()->mutable_io_engine();
      Configure(storage, engine, path);
      const auto response = broker().HandleRequest(request);
      EXPECT_TRUE(response.has_failure());
      EXPECT_EQ(response.failure().code(), Failure::INVALID_REQUEST);
    }
  }
  EXPECT_FALSE(fs::exists(destination));
  EXPECT_TRUE(fs::exists(source_ / "image"));
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

TEST_F(BrokerTest, RejectsUnownedCheckpointStaging)
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
  EXPECT_EQ(failed.failure().code(), Failure::TRANSACTION_CONFLICT);

  auto abort = RequestFor("checkpoint");
  abort.mutable_abort();
  EXPECT_EQ(broker().HandleRequest(abort).failure().code(), Failure::TRANSACTION_NOT_FOUND);
  EXPECT_TRUE(fs::is_symlink(staging_directory));

  const auto retry = broker().HandleRequest(prepare);
  ASSERT_TRUE(retry.has_failure());
  EXPECT_EQ(retry.failure().code(), Failure::TRANSACTION_CONFLICT);
}

TEST_F(BrokerTest, PublishedCheckpointRootPreservesCapturePermissions)
{
  // Use normal directory creation as the staged-mode reference without changing
  // the process-wide umask while broker threads are running.
  const auto reference = root_ / "reference";
  ASSERT_TRUE(fs::create_directory(reference));
  const auto staged_permissions = fs::status(reference).permissions();
  for (const bool direct : {false, true}) {
    SCOPED_TRACE(direct ? "direct" : "staged");
    const std::string id = direct ? "direct-mode" : "staged-mode";
    const auto destination = root_ / "storage" / id;
    auto prepare = RequestFor(id);
    auto* storage = direct ? prepare.mutable_prepare_direct_checkpoint()->mutable_destination()
                           : prepare.mutable_prepare_staged_checkpoint()->mutable_destination();
    auto* engine = direct ? prepare.mutable_prepare_direct_checkpoint()->mutable_io_engine()
                          : prepare.mutable_prepare_staged_checkpoint()->mutable_io_engine();
    Configure(storage, engine, destination);
    const auto prepared = broker().HandleRequest(prepare);
    ASSERT_FALSE(prepared.has_failure()) << prepared.DebugString();
    const fs::path capture = direct ? prepared.direct_checkpoint_directory().image_directory()
                                    : prepared.staged_checkpoint_directory().image_directory();
    const auto expected_permissions = direct ? fs::perms::owner_all : staged_permissions;
    EXPECT_EQ(fs::status(capture).permissions(), expected_permissions);
    std::ofstream(capture / "image") << "image";

    auto commit = RequestFor(id);
    commit.mutable_commit();
    const auto published = broker().HandleRequest(commit);
    ASSERT_TRUE(published.has_commit_complete()) << published.DebugString();
    EXPECT_EQ(fs::status(destination).permissions(), expected_permissions);
    EXPECT_TRUE(fs::exists(destination / "image"));
  }
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

TEST_F(BrokerTest, DirectPublicationRejectsConflictWithoutChangingEitherTree)
{
  auto engine = std::make_unique<PosixCopyEngine>(root_ / "storage");
  const auto published = root_ / "storage/published";
  const fs::path partial = published.string() + ".pagebroker-partial";
  fs::create_directory(published);
  fs::create_directory(partial);
  std::ofstream(published / "old") << "old";
  std::ofstream(partial / "other-attempt") << "keep";
  StorageBackend destination;
  destination.mutable_filesystem()->set_directory(published.string());
  try {
    engine->PublishCheckpoint(source_, destination, engine->InspectCheckpoint(source_), nullptr, {}, TransferMode::Direct);
    FAIL() << "publication should reject an existing partial";
  }
  catch (const TransferError& error) {
    EXPECT_EQ(error.code, Failure::TRANSACTION_CONFLICT);
  }
  EXPECT_TRUE(fs::exists(source_ / "image"));
  EXPECT_TRUE(fs::exists(published / "old"));
  EXPECT_FALSE(fs::exists(published / "image"));
  EXPECT_FALSE(fs::exists(partial / "image"));
  std::string contents;
  std::ifstream(partial / "other-attempt") >> contents;
  EXPECT_EQ(contents, "keep");
}

TEST_F(BrokerTest, PreservesPartialFileWhenCheckpointDestinationIsAbsent)
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
  const fs::path staging(output.staged_checkpoint_directory().image_directory());
  EXPECT_TRUE(fs::exists(staging / "image"));
  EXPECT_FALSE(fs::exists(destination));

  fs::remove(partial);
  EXPECT_TRUE(broker().HandleRequest(commit).has_commit_complete());
  EXPECT_TRUE(fs::exists(destination / "image"));
  EXPECT_FALSE(fs::exists(staging));
}

TEST_F(BrokerTest, PreservesExistingPartialCheckpointDestination)
{
  for (const bool direct : {false, true}) {
    for (const std::string kind : {"file", "directory", "symlink"}) {
      for (const bool retry : {false, true}) {
        const auto id = std::string(direct ? "direct-" : "staged-") + kind + (retry ? "-retry" : "-abort");
        SCOPED_TRACE(id);
        const fs::path destination = root_ / "storage" / id;
        fs::create_directory(destination);
        std::ofstream(destination / "old") << "old";
        auto prepare = RequestFor(id);
        if (direct) {
          Configure(prepare.mutable_prepare_direct_checkpoint()->mutable_destination(),
                    prepare.mutable_prepare_direct_checkpoint()->mutable_io_engine(), destination);
        } else {
          Configure(prepare.mutable_prepare_staged_checkpoint()->mutable_destination(),
                    prepare.mutable_prepare_staged_checkpoint()->mutable_io_engine(), destination);
        }
        const auto output = broker().HandleRequest(prepare);
        ASSERT_FALSE(output.has_failure()) << output.DebugString();
        const fs::path directory = direct ? output.direct_checkpoint_directory().image_directory()
                                         : output.staged_checkpoint_directory().image_directory();
        std::ofstream(directory / "image") << "image";
        const fs::path partial = destination.string() + ".pagebroker-partial";
        if (kind == "directory") {
          fs::create_directory(partial);
          std::ofstream(partial / "other-writer") << "keep";
        } else if (kind == "symlink") {
          fs::create_symlink(root_ / "missing", partial);
        } else {
          std::ofstream(partial) << "keep";
        }
        auto commit = RequestFor(id);
        commit.mutable_commit();
        const auto response = broker().HandleRequest(commit);
        ASSERT_TRUE(response.has_failure());
        EXPECT_EQ(response.failure().code(), Failure::TRANSACTION_CONFLICT);
        EXPECT_TRUE(fs::exists(directory / "image"));
        EXPECT_TRUE(fs::exists(destination / "old"));
        EXPECT_FALSE(fs::exists(destination / "image"));
        if (kind == "symlink") {
          EXPECT_EQ(fs::read_symlink(partial), root_ / "missing");
        } else {
          std::string contents;
          std::ifstream(kind == "directory" ? partial / "other-writer" : partial) >> contents;
          EXPECT_EQ(contents, "keep");
        }
        if (retry) {
          fs::remove_all(partial);
          EXPECT_TRUE(broker().HandleRequest(commit).has_commit_complete());
          EXPECT_TRUE(fs::exists(destination / "image"));
          EXPECT_FALSE(fs::exists(destination / "old"));
        } else {
          auto abort = RequestFor(id);
          abort.mutable_abort();
          EXPECT_TRUE(broker().HandleRequest(abort).has_abort_complete());
          EXPECT_TRUE(fs::exists(fs::symlink_status(partial)));
          EXPECT_TRUE(fs::exists(destination / "old"));
        }
        EXPECT_FALSE(fs::exists(directory));
      }
    }
  }
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
  const auto retry = RequestFor("direct-save");
  ASSERT_NE(retry.request_id(), request.request_id());
  request.set_request_id(retry.request_id());
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
  const auto access = engine.StageRestore(engine.PrepareRestore(source), cpu);
  EXPECT_EQ(access.image_directory, cpu);
  EXPECT_EQ(access.staging_directory, cpu);
  struct stat original{}, retained{};
  ASSERT_EQ(stat(source_.c_str(), &original), 0);
  ASSERT_EQ(fstat(access.source.get(), &retained), 0);
  EXPECT_EQ(original.st_ino, retained.st_ino);
  EXPECT_TRUE(fs::exists(cpu / "image"));
  EXPECT_FALSE(fs::exists(cpu / gpu::kDataDirectory));
  EXPECT_TRUE(fs::exists(source_ / gpu::kDataDirectory / "1" / "extent"));
  EXPECT_EQ(engine.PrepareRestore(source).inventory.size_bytes(), fs::file_size(source_ / "image"));
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


}  // namespace

TEST_F(BrokerTest, RejectsArtifactCheckpointBeforeStagingThenAllowsLegacyFallback)
{
  auto request = RequestFor("artifact-checkpoint");
  auto* operation = request.mutable_prepare_staged_checkpoint();
  auto* target = operation->mutable_target();
  target->set_store_id("store-v1-" + std::string(64, 'a'));
  target->mutable_artifact()->set_artifact_uid("content-1");
  target->mutable_artifact()->set_container_name("main");
  const auto rejected = broker().HandleRequest(request);
  ASSERT_TRUE(rejected.has_failure());
  EXPECT_EQ(rejected.failure().code(), Failure::INVALID_REQUEST);
  EXPECT_EQ(rejected.request_id(), request.request_id());
  EXPECT_EQ(rejected.transaction_id(), request.transaction_id());
  EXPECT_FALSE(fs::exists(root_ / "tmpfs" / "checkpoint" / request.transaction_id()));
  EXPECT_FALSE(fs::exists(root_ / "storage" / "output"));

  // Rejection did not allocate a transaction; a legacy call on the same ID succeeds.
  // selector is a oneof, so setting destination here also clears target.
  Configure(operation->mutable_destination(), operation->mutable_io_engine(), root_ / "storage" / "output");
  EXPECT_TRUE(broker().HandleRequest(request).has_staged_checkpoint_directory());
}

TEST_F(BrokerTest, RejectsArtifactRestoreBeforeReadingThenAllowsLegacyFallback)
{
  auto request = RequestFor("artifact-restore");
  auto* operation = request.mutable_staged_restore();
  auto* artifact = operation->mutable_artifact();
  artifact->set_store_id("store-v1-" + std::string(64, 'a'));
  artifact->set_artifact_handle("artifacts/content-1/containers/main");
  const auto rejected = broker().HandleRequest(request);
  ASSERT_TRUE(rejected.has_failure());
  EXPECT_EQ(rejected.failure().code(), Failure::INVALID_REQUEST);
  EXPECT_FALSE(fs::exists(root_ / "tmpfs" / "restore" / request.transaction_id()));
  EXPECT_TRUE(fs::exists(source_ / "image"));
  // selector is a oneof, so setting source here also clears artifact.
  Configure(operation->mutable_source(), operation->mutable_io_engine(), source_);
  EXPECT_TRUE(broker().HandleRequest(request).has_staged_restore_directory());
}

TEST_F(BrokerTest, RejectsInvalidS3DescriptorsBeforeAllocatingRestoreOrMetadataTransactions)
{
  broker_.emplace(root_ / "tmpfs", root_ / "storage", nullptr, TestS3Config());
  struct Case {
    const char* name;
    const char* store;
    std::string handle;
    Failure::Code code;
  };
  for (const auto& invalid : {
      Case{"missing-store", "", std::string(64, 'a'), Failure::INVALID_REQUEST},
      Case{"malformed-handle", "test-store", "../foreign", Failure::INVALID_REQUEST},
      Case{"foreign-store", "other-store", std::string(64, 'a'), Failure::STORE_MISMATCH}}) {
    SCOPED_TRACE(invalid.name);
    for (const bool metadata : {false, true}) {
      SCOPED_TRACE(metadata ? "metadata" : "restore");
      auto request = RequestFor(invalid.name);
      auto* artifact = metadata ? request.mutable_get_artifact_metadata()->mutable_artifact()
                                : request.mutable_staged_restore()->mutable_artifact();
      artifact->set_store_id(invalid.store);
      artifact->set_artifact_handle(invalid.handle);
      const auto rejected = broker().HandleRequest(request);
      ASSERT_TRUE(rejected.has_failure());
      EXPECT_EQ(rejected.failure().code(), invalid.code);
      EXPECT_FALSE(fs::exists(root_ / "tmpfs" / "restore" / request.transaction_id()));
      request.mutable_commit();
      EXPECT_EQ(broker().HandleRequest(request).failure().code(), Failure::TRANSACTION_NOT_FOUND);
    }
  }
}

TEST_F(BrokerTest, RejectsUnsupportedArtifactSelectorsWithOrWithoutEngine)
{
  auto request = RequestFor("empty-target");
  request.mutable_prepare_staged_checkpoint()->mutable_target();
  EXPECT_EQ(broker().HandleRequest(request).failure().code(), Failure::INVALID_REQUEST);
  request.mutable_prepare_staged_checkpoint()->mutable_io_engine()->mutable_posix_copy();
  EXPECT_EQ(broker().HandleRequest(request).failure().code(), Failure::INVALID_REQUEST);

  request = RequestFor("empty-artifact");
  request.mutable_staged_restore()->mutable_artifact();
  EXPECT_EQ(broker().HandleRequest(request).failure().code(), Failure::INVALID_REQUEST);
  request.mutable_staged_restore()->mutable_io_engine()->mutable_posix_copy();
  EXPECT_EQ(broker().HandleRequest(request).failure().code(), Failure::INVALID_REQUEST);

  request = RequestFor("metadata-subject");
  request.mutable_get_artifact_metadata();
  EXPECT_EQ(broker().HandleRequest(request).failure().message(), "metadata subject is required");
}

TEST_F(BrokerTest, MetadataIsExplicitlyUnsupportedWithoutCreatingTransaction)
{
  auto request = RequestFor("metadata");
  auto* artifact = request.mutable_get_artifact_metadata()->mutable_artifact();
  artifact->set_store_id("store-v1-" + std::string(64, 'a'));
  artifact->set_artifact_handle("artifacts/content-1/containers/main");
  const auto rejected = broker().HandleRequest(request);
  ASSERT_TRUE(rejected.has_failure());
  EXPECT_EQ(rejected.failure().code(), Failure::INVALID_REQUEST);
  EXPECT_EQ(rejected.failure().message(), "configured I/O engine not found");
  EXPECT_EQ(rejected.request_id(), request.request_id());
  EXPECT_EQ(rejected.transaction_id(), request.transaction_id());

  auto* target = request.mutable_get_artifact_metadata()->mutable_target();
  target->set_store_id("store-v1-" + std::string(64, 'a'));
  target->mutable_artifact()->set_artifact_uid("content-1");
  target->mutable_artifact()->set_container_name("main");
  EXPECT_EQ(broker().HandleRequest(request).failure().code(), Failure::INVALID_REQUEST);

  request.mutable_commit();
  EXPECT_EQ(broker().HandleRequest(request).failure().code(), Failure::TRANSACTION_NOT_FOUND);
  EXPECT_TRUE(fs::exists(source_ / "image"));
}

TEST_F(BrokerTest, CommitAndAbortReportExpiredTransactions)
{
  auto request = RequestFor("expired-live");
  auto* operation = request.mutable_prepare_staged_checkpoint();
  Configure(operation->mutable_destination(), operation->mutable_io_engine(), root_ / "storage" / "output");
  ASSERT_TRUE(broker().HandleRequest(request).has_staged_checkpoint_directory());

  const auto later = std::chrono::steady_clock::now() + std::chrono::hours(2) + std::chrono::minutes(5);
  broker().SetClock([later] { return later; });
  request.mutable_commit();
  EXPECT_EQ(broker().HandleRequest(request).failure().code(), Failure::TRANSACTION_EXPIRED);
  request.mutable_abort();
  EXPECT_EQ(broker().HandleRequest(request).failure().code(), Failure::TRANSACTION_EXPIRED);
  EXPECT_FALSE(fs::exists(root_ / "storage" / "output"));

  broker().ReapExpiredTransactions(later);
  request.mutable_commit();
  EXPECT_EQ(broker().HandleRequest(request).failure().code(), Failure::TRANSACTION_NOT_FOUND);
}

TEST(DaemonOptionsTest, RejectsTransactionLifetimeFlag)
{
  const std::array<std::string_view, 5> arguments{"socket", "staging", "storage", "--transaction-lifetime", "17"};
  EXPECT_THROW(ParseDaemonOptions(arguments), std::invalid_argument);
}

TEST(DaemonOptionsTest, RejectsDuplicateFlags)
{
  const std::array<std::string_view, 7> duplicate{
      "socket", "staging", "storage", "--max-concurrent-requests", "17", "--max-concurrent-requests", "18"};
  EXPECT_THROW(ParseDaemonOptions(duplicate), std::invalid_argument);
}

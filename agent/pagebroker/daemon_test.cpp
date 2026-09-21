// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include <gtest/gtest.h>

#include <filesystem>
#include <future>
#include <fstream>
#include <optional>
#include <string>
#include <thread>

#include "broker.hpp"
#include "native_session.hpp"
#include <signal.h>
#include "posix_copy_engine.hpp"
#include <fcntl.h>
#include <unistd.h>

namespace fs = std::filesystem;
using namespace snapshot::pagebroker;

namespace {

class BrokerTest : public ::testing::Test {
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

  Request RequestFor(const std::string& id)
  {
    Request request;
    request.set_request_id("request-" + id + "-" + std::to_string(++request_number_));
    request.set_transaction_id(id);
    return request;
  }

  void Configure(StorageBackend* storage, IOEngine* engine, const fs::path& directory)
  {
    storage->mutable_filesystem()->set_directory(directory.string());
    engine->mutable_posix_copy();
  }

  Broker& broker() { return *broker_; }

  fs::path root_;
  fs::path source_;
  std::optional<Broker> broker_;
  unsigned request_number_ = 0;
};

TEST_F(BrokerTest, NativeSessionsBindTransactionAndRejectPrematureComplete)
{
  Broker native(root_ / "native-staging", root_ / "storage", "/unused/pagebroker-gpu-engine");
  auto prepare = RequestFor("native");
  Configure(prepare.mutable_prepare_staged_checkpoint()->mutable_destination(),
            prepare.mutable_prepare_staged_checkpoint()->mutable_io_engine(), root_ / "storage" / "native");
  ASSERT_TRUE(native.HandleRequest(prepare).has_staged_checkpoint_directory());
  auto binding = RequestFor("native");
  auto* request = binding.mutable_bind_native();
  request->set_direction(v1::BindNativeSession::SAVE);
  request->set_container_pid(getpid());
  request->set_namespace_pid(getpid());
  request->add_visible_devices("GPU-00000000-0000-0000-0000-000000000001");
  auto session = native.BindNative(binding);
  EXPECT_THROW(native.BindNative(binding), std::runtime_error);
  auto commit = RequestFor("native");
  commit.mutable_commit();
  EXPECT_TRUE(native.HandleRequest(commit).has_failure());
  v1::NativeSessionRequest complete;
  complete.set_operation(v1::NativeSessionRequest::COMPLETE);
  EXPECT_TRUE(session->Execute(complete).has_failure());
  EXPECT_TRUE(native.HandleRequest(commit).has_failure());
  auto abort = RequestFor("native");
  abort.mutable_abort();
  auto pending = std::async(std::launch::async, [&] { return native.HandleRequest(abort); });
  EXPECT_EQ(pending.wait_for(std::chrono::milliseconds(100)), std::future_status::timeout);
  // The wait releases the transaction mutex, so teardown can return admission.
  session.reset();
  ASSERT_EQ(pending.wait_for(std::chrono::seconds(1)), std::future_status::ready);
  EXPECT_TRUE(pending.get().has_abort_complete());
  EXPECT_TRUE(fs::is_empty(root_ / "native-staging" / "checkpoint"));
  EXPECT_TRUE(native.HandleRequest(abort).has_abort_complete());
}

TEST_F(BrokerTest, NativeEngineReusesProcessAndHoldsAdmissionUntilDrain)
{
  fs::create_symlink(fs::absolute("build/fake-gpu-engine"), root_ / "pagebroker-gpu-engine");
  Broker native(root_ / "native-staging", root_ / "storage", root_ / "pagebroker-gpu-engine");
  native.StartGpuEngine();
  int engine = 0;
  for (int index = 0; index < 2; ++index) {
    const auto target = std::to_string(999998 + index);
    const auto directory = source_ / "native" / target;
    fs::create_directories(directory);
    const auto id = "native-load-" + target;
    auto restore = RequestFor(id);
    restore.mutable_direct_restore()->mutable_source()->mutable_filesystem()->set_directory(source_.string());
    restore.mutable_direct_restore()->mutable_io_engine()->mutable_posix_copy();
    ASSERT_TRUE(native.HandleRequest(restore).has_direct_restore_ready());
    auto binding = RequestFor(id);
    auto* request = binding.mutable_bind_native();
    request->set_direction(v1::BindNativeSession::LOAD);
    request->set_container_pid(getpid());
    request->set_namespace_pid(std::stoi(target));
    request->add_visible_devices("GPU-00000000-0000-0000-0000-000000000001");
    auto session = native.BindNative(binding);
    v1::NativeSessionRequest prepare;
    prepare.set_operation(v1::NativeSessionRequest::PREPARE);
    prepare.set_target_pid(getpid());
    ASSERT_FALSE(session->Execute(prepare).has_failure());
    int observed = 0;
    std::ifstream(directory / "engine-pid") >> observed;
    if (!engine) engine = observed;
    EXPECT_EQ(engine, observed);
    std::ifstream(directory / "target-pid") >> observed;
    EXPECT_EQ(observed, getpid());
    auto abort = RequestFor(id);
    abort.mutable_abort();
    auto pending = std::async(std::launch::async, [&] { return native.HandleRequest(abort); });
    auto closing = std::async(std::launch::async, [&] { session.reset(); });
    EXPECT_EQ(closing.wait_for(std::chrono::milliseconds(100)), std::future_status::timeout);
    EXPECT_EQ(pending.wait_for(std::chrono::milliseconds(100)), std::future_status::timeout);
    std::ofstream(directory / "allow-drain") << "true";
    closing.get();
    EXPECT_TRUE(pending.get().has_abort_complete());
    EXPECT_EQ(kill(engine, 0), 0);
    EXPECT_TRUE(fs::exists(source_ / "image"));
  }
}

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
  const fs::path published = root_ / "storage" / "published";
  const fs::path previous = published.string() + ".pagebroker-previous";
  fs::create_directories(published);
  std::ofstream(published / "old") << "old";

  auto prepare = RequestFor("checkpoint");
  Configure(
      prepare.mutable_prepare_staged_checkpoint()->mutable_destination(),
      prepare.mutable_prepare_staged_checkpoint()->mutable_io_engine(), published);
  const auto output = broker().HandleRequest(prepare);
  ASSERT_TRUE(output.has_staged_checkpoint_directory());
  std::ofstream(fs::path(output.staged_checkpoint_directory().image_directory()) / "new") << "new";

  auto commit = RequestFor("checkpoint");
  commit.mutable_commit();
  EXPECT_TRUE(broker().HandleRequest(commit).has_commit_complete());
  EXPECT_FALSE(fs::exists(published / "old"));
  EXPECT_TRUE(fs::exists(published / "new"));
  EXPECT_FALSE(fs::exists(previous));
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

TEST_F(BrokerTest, DirectRestoreNeverStagesOrDeletesSource)
{
  for (const std::string finish : {"commit", "abort", "expire"}) {
    auto restore = RequestFor(finish);
    Configure(restore.mutable_direct_restore()->mutable_source(),
              restore.mutable_direct_restore()->mutable_io_engine(), source_);
    ASSERT_TRUE(broker().HandleRequest(restore).has_direct_restore_ready());
    EXPECT_TRUE(fs::is_empty(root_ / "tmpfs" / "restore"));
    EXPECT_EQ(broker().HandleRequest(restore).failure().code(), Failure::TRANSACTION_CONFLICT);
    auto terminal = RequestFor(finish);
    if (finish == "commit") {
      terminal.mutable_commit();
      EXPECT_TRUE(broker().HandleRequest(terminal).has_commit_complete());
      EXPECT_TRUE(broker().HandleRequest(terminal).has_commit_complete());
    } else if (finish == "abort") {
      terminal.mutable_abort();
      EXPECT_TRUE(broker().HandleRequest(terminal).has_abort_complete());
      EXPECT_TRUE(broker().HandleRequest(terminal).has_abort_complete());
    } else {
      broker().ReapExpiredTransactions(std::chrono::steady_clock::now() + std::chrono::hours(3));
      terminal.mutable_commit();
      EXPECT_EQ(broker().HandleRequest(terminal).failure().code(), Failure::TRANSACTION_NOT_FOUND);
    }
    std::string contents;
    std::ifstream(source_ / "image") >> contents;
    EXPECT_EQ(contents, "image");
  }
}

TEST_F(BrokerTest, DirectRestoreValidatesBeforeReservingTransaction)
{
  auto request = RequestFor("direct");
  request.mutable_direct_restore();
  EXPECT_EQ(broker().HandleRequest(request).failure().code(), Failure::INVALID_REQUEST);
  request.mutable_direct_restore()->mutable_source()->mutable_filesystem()->set_directory(source_.string());
  EXPECT_EQ(broker().HandleRequest(request).failure().code(), Failure::INVALID_REQUEST);
  request.mutable_direct_restore()->mutable_io_engine()->mutable_posix_copy();
  fs::create_symlink(source_ / "image", source_ / "link");
  EXPECT_TRUE(broker().HandleRequest(request).has_failure());
  fs::remove(source_ / "link");
  request.mutable_direct_restore()->mutable_source()->mutable_filesystem()->set_directory(root_.string());
  EXPECT_TRUE(broker().HandleRequest(request).has_failure());
  request.mutable_direct_restore()->mutable_source()->mutable_filesystem()->set_directory(source_.string());
  EXPECT_TRUE(broker().HandleRequest(request).has_direct_restore_ready());
}

TEST_F(BrokerTest, DirectConsumerReadsRetainedDirectoryAfterRename)
{
  StorageBackend source;
  source.mutable_filesystem()->set_directory(source_.string());
  PosixCopyEngine engine(root_ / "storage");
  auto directory = engine.OpenRestoreSource(source);
  EXPECT_EQ(fcntl(directory.get(), F_GETFL) & O_ACCMODE, O_RDONLY);
  fs::rename(source_, root_ / "storage" / "renamed");
  fs::create_directories(source_);
  std::ofstream(source_ / "image") << "replacement";
  FileDescriptor image(openat(directory.get(), "image", O_RDONLY | O_NOFOLLOW));
  ASSERT_GE(image.get(), 0);
  char bytes[5];
  ASSERT_EQ(read(image.get(), bytes, sizeof(bytes)), sizeof(bytes));
  EXPECT_EQ(std::string(bytes, sizeof(bytes)), "image");
}

}  // namespace

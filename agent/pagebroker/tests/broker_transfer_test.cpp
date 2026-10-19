// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "broker.hpp"

#include <gtest/gtest.h>
#include <sys/stat.h>

#include <exception>
#include <fstream>
#include <functional>
#include <future>
#include <string>
#include <system_error>
#include <thread>

#include "tests/request_builder.hpp"
#include "tests/temporary_directory.hpp"
#include "transfer/engine/posix/posix_copy_engine.hpp"
#include "transfer/filesystem/filesystem_storage.hpp"

namespace {
// Interrupt transaction directory creation without changing production code. Each
// worker owns its hook; unrelated filesystem operations remain unchanged.
thread_local std::function<void(const char*)> mkdir_hook;
class ScopedMkdirHook {
 public:
  explicit ScopedMkdirHook(std::function<void(const char*)> hook)
  { mkdir_hook = std::move(hook); }
  ~ScopedMkdirHook() { mkdir_hook = {}; }
};
}  // namespace

extern "C" int __real_mkdir(const char*, mode_t);
extern "C" int __wrap_mkdir(const char* path, mode_t mode)
{
  const int result = __real_mkdir(path, mode);
  if (result == 0 && mkdir_hook)
    mkdir_hook(path);
  return result;
}

namespace snapshot::pagebroker {
namespace fs = std::filesystem;
using test::Configure;
namespace {
enum class Boundary { PrepareRestore, StageRestore, ValidateCheckpoint, InspectCheckpoint, PublishCheckpoint };

// Real filesystem operations with deterministic interruption at engine boundaries.
class InterruptingEngine : public TransferEngine {
 public:
  using Callback = std::function<void(Boundary, Cancellation)>;
  InterruptingEngine(const Path& storage, Callback callback) : posix_(storage), callback_(std::move(callback)) {}
  IoEngine type() const override { return IoEngine::POSIX_COPY; }
  PreparedRestore PrepareRestore(const StorageBackend& source, Cancellation control,
      const PublishedArtifact* artifact, bool metadata_only, TransferMode mode) const override
  {
    callback_(Boundary::PrepareRestore, control);
    return posix_.PrepareRestore(source, control, artifact, metadata_only, mode);
  }
  RestoreAccess StageRestore(PreparedRestore preparation, const Path& destination, Cancellation control) const override
  {
    auto access = posix_.StageRestore(std::move(preparation), destination, control);
    callback_(Boundary::StageRestore, control);
    return access;
  }
  Path PrepareCheckpoint(const StorageBackend& destination, const Path& staging_directory,
      const std::string& transaction_id, TransferMode mode, const PublishedArtifact* artifact,
      Cancellation control) const override
  {
    ValidateCheckpointDestination(destination, artifact, control);
    return posix_.PrepareCheckpoint(destination, staging_directory, transaction_id, mode, artifact, control);
  }
  void ValidateCheckpointDestination(const StorageBackend& destination,
      const PublishedArtifact* artifact, Cancellation control) const override
  {
    callback_(Boundary::ValidateCheckpoint, control);
    posix_.ValidateCheckpointDestination(destination, artifact, control);
  }
  RestorePlan InspectCheckpoint(const Path& source, const PublishedArtifact* artifact,
      Cancellation control) const override
  {
    callback_(Boundary::InspectCheckpoint, control);
    return posix_.InspectCheckpoint(source, artifact, control);
  }
  void PublishCheckpoint(const Path& source, const StorageBackend& destination, RestorePlan plan,
      CheckpointPublication* publication, Cancellation control, TransferMode mode) const override
  {
    callback_(Boundary::PublishCheckpoint, control);
    posix_.PublishCheckpoint(source, destination, std::move(plan), publication, control, mode);
  }

 private:
  PosixCopyEngine posix_;
  Callback callback_;
};
}  // namespace

TEST(FilesystemStorageTest, ExclusiveCreationPreservesEveryExistingEntry)
{
  test::TemporaryDirectory root;
  fs::create_directory(root.path() / "directory");
  std::ofstream(root.path() / "directory/marker") << "keep";
  std::ofstream(root.path() / "file") << "keep";
  fs::create_directory_symlink(root.path() / "directory", root.path() / "live");
  fs::create_directory_symlink(root.path() / "missing", root.path() / "dangling");
  for (const auto* name : {"directory", "file", "live", "dangling"}) {
    SCOPED_TRACE(name);
    const auto path = root.path() / name;
    const auto before = fs::symlink_status(path);
    try {
      filesystem_storage::CreateDirectoryExclusive(path, fs::perms::owner_all);
      FAIL() << "existing entry was accepted";
    }
    catch (const TransferError& error) {
      EXPECT_EQ(error.code, Failure::TRANSACTION_CONFLICT);
    }
    EXPECT_EQ(fs::symlink_status(path).type(), before.type());
    EXPECT_EQ(fs::symlink_status(path).permissions(), before.permissions());
  }
  EXPECT_EQ(fs::file_size(root.path() / "directory/marker"), 4u);
  EXPECT_EQ(fs::file_size(root.path() / "file"), 4u);
  EXPECT_EQ(fs::read_symlink(root.path() / "live"), root.path() / "directory");
  EXPECT_EQ(fs::read_symlink(root.path() / "dangling"), root.path() / "missing");
  EXPECT_FALSE(fs::exists(root.path() / "missing"));
}

TEST(FilesystemStorageTest, ExclusiveCreationRespectsRequestedPermissionsAndUmask)
{
  for (const auto permissions : {fs::perms::all, fs::perms::owner_all}) {
    SCOPED_TRACE(static_cast<unsigned>(permissions));
    test::TemporaryDirectory root;
    fs::create_directory(root.path() / "reference");
    const auto expected = fs::status(root.path() / "reference").permissions() & permissions;
    filesystem_storage::CreateDirectoryExclusive(root.path() / "created", permissions);
    EXPECT_TRUE(fs::is_directory(root.path() / "created"));
    EXPECT_EQ(fs::status(root.path() / "created").permissions() & fs::perms::all, expected);
  }
}

TEST(FilesystemStorageTest, ExclusiveCreationPreservesNonConflictErrors)
{
  test::TemporaryDirectory root;
  try {
    filesystem_storage::CreateDirectoryExclusive(root.path() / "missing/child", fs::perms::all);
    FAIL() << "missing parent was accepted";
  }
  catch (const std::system_error& error) {
    EXPECT_EQ(error.code(), std::errc::no_such_file_or_directory);
    EXPECT_EQ(TransferFailureCode(error), Failure::STORAGE_ERROR);
  }
  EXPECT_FALSE(fs::exists(root.path() / "missing"));
}

TEST(FilesystemStorageTest, AppliesOnlyKnownRestorePermissions)
{
  for (bool known : {false, true}) {
    SCOPED_TRACE(known);
    test::TemporaryDirectory root;
    const auto source = root.path() / "source";
    const auto destination = root.path() / "restored";
    std::ofstream(source) << "image";
    const auto file_mode = fs::perms::owner_read | fs::perms::owner_write;
    fs::permissions(source, file_mode);
    fs::create_directory(destination);
    fs::permissions(destination, fs::perms::owner_all);
    fs::create_directory(root.path() / "reference");
    const auto created_mode = fs::status(root.path() / "reference").permissions();

    RestorePlan plan;
    plan.directories.push_back(RestoreDirectory{"nested"});
    plan.files.push_back(RestoreFile{source.string(), "nested/image", 5});
    const auto root_mode = fs::perms::owner_all | fs::perms::group_read | fs::perms::group_exec;
    if (known) {
      plan.root_permissions = root_mode;
      plan.directories.front().permissions = fs::perms::owner_all;
      plan.files.front().permissions = fs::perms::owner_read;
    }

    filesystem_storage::StageRestore(plan, destination, {});
    EXPECT_EQ(fs::status(destination).permissions(), known ? root_mode : fs::perms::owner_all);
    EXPECT_EQ(fs::status(destination / "nested").permissions(), known ? fs::perms::owner_all : created_mode);
    EXPECT_EQ(fs::status(destination / "nested/image").permissions(), known ? fs::perms::owner_read : file_mode);
    EXPECT_EQ(fs::file_size(destination / "nested/image"), 5u);
  }
}

class BrokerTransferTest : public ::testing::Test, public test::RequestBuilder {
 protected:
  void SetUp() override
  {
    fs::create_directories(source_);
    std::ofstream(source_ / "image") << "image";
  }

  void Intercept(Broker& broker, InterruptingEngine::Callback callback)
  {
    broker.io_engines_.clear();
    broker.io_engines_.push_back(std::make_unique<InterruptingEngine>(storage_, std::move(callback)));
  }

  Request Restore()
  {
    auto request = RequestFor("restore");
    Configure(request.mutable_staged_restore()->mutable_source(), request.mutable_staged_restore()->mutable_io_engine(),
        source_);
    return request;
  }

  Request Checkpoint(bool direct)
  {
    auto request = RequestFor("checkpoint");
    auto* destination = direct ? request.mutable_prepare_direct_checkpoint()->mutable_destination()
                               : request.mutable_prepare_staged_checkpoint()->mutable_destination();
    auto* engine = direct ? request.mutable_prepare_direct_checkpoint()->mutable_io_engine()
                          : request.mutable_prepare_staged_checkpoint()->mutable_io_engine();
    Configure(destination, engine, storage_ / "published");
    return request;
  }

  Path CheckpointOutput(bool direct)
  {
    return direct ? storage_ / "published.pagebroker-tx-checkpoint" : staging_ / "checkpoint/checkpoint";
  }

  std::shared_ptr<TransactionResources> Limit(Broker& broker, uintmax_t bytes = 4, unsigned transactions = 1)
  {
    broker.resources_ = std::make_shared<TransactionResources>(bytes, transactions);
    return broker.resources_;
  }

  std::shared_ptr<Transaction> Find(Broker& broker, const std::string& id)
  {
    return broker.FindTransaction(id);
  }

  std::shared_ptr<Transaction> Unprepared(Broker& broker, const std::string& id)
  {
    return broker.CreateOrGetTransaction(id);
  }

  void Erase(Broker& broker, const std::string& id, const std::shared_ptr<Transaction>& transaction)
  {
    broker.EraseTransaction(id, transaction);
  }

  void SetReservedStaging(Broker& broker, uintmax_t bytes)
  {
    std::lock_guard lock(broker.transactions_mutex_);
    broker.reserved_staging_bytes_ = bytes;
  }

  void SetLifetime(Broker& broker, std::chrono::seconds lifetime)
  {
    broker.lifetime_ = lifetime;
  }

  std::shared_ptr<Transaction> Seed(Broker& broker, uintmax_t initial_bytes, Transaction::State state)
  {
    auto transaction = broker.CreateOrGetTransaction("seed");
    std::lock_guard lock(transaction->mutex());
    transaction->PrepareTransfer(broker.resources_, initial_bytes, nullptr, broker.lifetime_);
    transaction->Reserve(4);
    fs::create_directory(staging_ / "restore/seed");
    transaction->OwnDirectory(staging_ / "restore/seed");
    std::ofstream(staging_ / "restore/seed/image") << "data";
    transaction->set_descriptor(RestoreTransactionDescriptor(staging_ / "restore/seed"));
    transaction->set_state(state);
    return transaction;
  }

  void TryRetention(Broker& broker) { broker.RetainTerminalTransaction("seed"); }

  size_t Retained(Broker& broker)
  {
    std::lock_guard lock(broker.terminal_transactions_mutex_);
    return broker.terminal_transactions_.size();
  }

  void BlockCleanup()
  {
    fs::rename(staging_ / "restore", staging_ / "held");
    std::ofstream(staging_ / "restore") << "blocks traversal";
  }

  void UnblockCleanup()
  {
    fs::remove(staging_ / "restore");
    fs::rename(staging_ / "held", staging_ / "restore");
  }

  test::TemporaryDirectory root_;
  const Path storage_ = root_.path() / "storage";
  const Path source_ = storage_ / "source";
  const Path staging_ = root_.path() / "staging";
};

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
      SCOPED_TRACE(static_cast<int>(boundary));
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
      EXPECT_TRUE(fs::exists(source_ / "image"));
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

TEST_F(BrokerTransferTest, DirectCheckpointRetryKeepsDeadlineAndCaptureOnRejection)
{
  for (const bool expire : {false, true}) {
    SCOPED_TRACE(expire);
    Broker broker(staging_, storage_);
    SetLifetime(broker, std::chrono::seconds(1));
    const auto request = Checkpoint(true);
    ASSERT_TRUE(broker.HandleRequest(request).has_direct_checkpoint_directory());
    const auto transaction = Find(broker, "checkpoint");
    const auto deadline = transaction->cancellation().Deadline();
    const auto output = CheckpointOutput(true);
    std::ofstream(output / "payload") << "capture";
    EXPECT_EQ(broker.HandleRequest(request).direct_checkpoint_directory().image_directory(), output.string());
    EXPECT_EQ(transaction->cancellation().Deadline(), deadline);
    auto changed = request;
    changed.mutable_prepare_direct_checkpoint()->mutable_destination()->mutable_filesystem()->set_directory(
        (storage_ / "other").string());
    EXPECT_EQ(broker.HandleRequest(changed).failure().code(), Failure::TRANSACTION_CONFLICT);
    EXPECT_TRUE(fs::exists(output / "payload"));
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

TEST_F(BrokerTransferTest, RestoreCommitSucceedsWhileCleanupIsPending)
{
  for (const bool limited : {false, true}) {
    SCOPED_TRACE(limited);
    Broker broker(staging_, storage_);
    const auto resources = limited ? Limit(broker) : nullptr;
    const auto transaction = Seed(broker, 0, Transaction::State::STAGED);
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
    const auto transaction = Seed(broker, 0, state);
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
  const auto original = Seed(broker, 0, Transaction::State::STAGED);
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
    auto transaction = Seed(broker, 0, state);
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
  for (const uintmax_t initial_bytes : {0u, 1u}) {
    SCOPED_TRACE(initial_bytes);
    Broker broker(staging_, storage_);
    SetLifetime(broker, std::chrono::minutes(1));
    const auto resources = Limit(broker);
    auto transaction = Seed(broker, initial_bytes, Transaction::State::STAGED);
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
    ASSERT_NO_THROW(next.PrepareTransfer(resources, initial_bytes));
    EXPECT_NO_THROW(next.Reserve(4));
    auto abort = RequestFor("seed");
    abort.mutable_abort();
    EXPECT_EQ(broker.HandleRequest(abort).failure().code(), Failure::TRANSACTION_NOT_FOUND);
    EXPECT_EQ(broker.HandleRequest(abort).failure().code(), Failure::TRANSACTION_NOT_FOUND);
  }
}

TEST_F(BrokerTransferTest, PeriodicCleanupRetriesKeepCapacityUntilRemovalSucceeds)
{
  for (const auto state : {Transaction::State::STAGED, Transaction::State::COMMITTED, Transaction::State::ABORTED}) {
    SCOPED_TRACE(static_cast<int>(state));
    Broker broker(staging_, storage_);
    SetLifetime(broker, std::chrono::minutes(1));
    const auto resources = Limit(broker);
    auto transaction = Seed(broker, 0, state);
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

TEST_F(BrokerTransferTest, AbortCleanupFailureRetainsStateAndAllowsRetry)
{
  for (const bool limited : {false, true}) {
    for (const auto state : {Transaction::State::STAGED, Transaction::State::ABORTED}) {
      SCOPED_TRACE(limited);
      SCOPED_TRACE(static_cast<int>(state));
      Broker broker(staging_, storage_);
      const auto resources = limited ? Limit(broker) : nullptr;
      const auto transaction = Seed(broker, 0, state);
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
  const auto transaction = Seed(broker, 0, Transaction::State::STAGED);
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
  auto transaction = Seed(broker, 0, Transaction::State::STAGED);
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
  auto transaction = Seed(broker, 0, Transaction::State::STAGED);
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

TEST_F(BrokerTransferTest, AbortAfterDeadlineLeavesCleanupToTheReaper)
{
  Broker broker(staging_, storage_);
  SetLifetime(broker, std::chrono::seconds(0));
  const auto resources = Limit(broker);
  auto transaction = Seed(broker, 0, Transaction::State::STAGED);
  auto abort = RequestFor("seed");
  abort.mutable_abort();
  BlockCleanup();
  EXPECT_EQ(broker.HandleRequest(abort).failure().code(), Failure::TRANSACTION_EXPIRED);
  EXPECT_EQ(transaction->state(), Transaction::State::STAGED);
  broker.ReapExpiredTransactions(std::chrono::steady_clock::now());
  EXPECT_EQ(transaction->state(), Transaction::State::ABORTING);
  EXPECT_EQ(resources->active, 1u);
  UnblockCleanup();
  EXPECT_EQ(broker.HandleRequest(abort).failure().code(), Failure::TRANSACTION_EXPIRED);
  EXPECT_TRUE(fs::exists(staging_ / "restore/seed/image"));
  broker.ReapExpiredTransactions(std::chrono::steady_clock::now());
  EXPECT_EQ(broker.HandleRequest(abort).failure().code(), Failure::TRANSACTION_NOT_FOUND);
  EXPECT_EQ(resources->active, 0u);
  EXPECT_EQ(resources->reserved, 0u);
  auto commit = RequestFor("seed");
  commit.mutable_commit();
  EXPECT_EQ(broker.HandleRequest(commit).failure().code(), Failure::TRANSACTION_NOT_FOUND);
}

TEST_F(BrokerTransferTest, ExpiredCommitLeavesCleanupAndItsRetriesToTheReaper)
{
  Broker broker(staging_, storage_);
  SetLifetime(broker, std::chrono::seconds(0));
  const auto resources = Limit(broker);
  auto transaction = Seed(broker, 0, Transaction::State::STAGED);
  auto commit = RequestFor("seed");
  commit.mutable_commit();
  BlockCleanup();
  EXPECT_EQ(broker.HandleRequest(commit).failure().code(), Failure::TRANSACTION_EXPIRED);
  EXPECT_EQ(transaction->state(), Transaction::State::STAGED);
  broker.ReapExpiredTransactions(std::chrono::steady_clock::now());
  EXPECT_EQ(transaction->state(), Transaction::State::ABORTING);
  EXPECT_EQ(resources->active, 1u);
  EXPECT_EQ(Retained(broker), 0u);
  UnblockCleanup();
  broker.ReapExpiredTransactions(std::chrono::steady_clock::now());
  EXPECT_EQ(transaction->state(), Transaction::State::ABORTED);
  EXPECT_EQ(resources->active, 0u);
  EXPECT_FALSE(fs::exists(staging_ / "restore/seed"));
  EXPECT_EQ(broker.HandleRequest(commit).failure().code(), Failure::TRANSACTION_NOT_FOUND);
}
}  // namespace snapshot::pagebroker

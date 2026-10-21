// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <gtest/gtest.h>

#include <chrono>
#include <filesystem>
#include <fstream>
#include <functional>
#include <memory>
#include <mutex>
#include <string>
#include <utility>

#include "broker.hpp"
#include "tests/request_builder.hpp"
#include "tests/temporary_directory.hpp"
#include "transfer/engine/posix/posix_copy_engine.hpp"

namespace snapshot::pagebroker::test {
enum class Boundary { PrepareRestore, StageRestore, ValidateCheckpoint, InspectCheckpoint, PublishCheckpoint };

inline const char* BoundaryName(Boundary boundary)
{
  switch (boundary) {
    case Boundary::PrepareRestore: return "PrepareRestore";
    case Boundary::StageRestore: return "StageRestore";
    case Boundary::ValidateCheckpoint: return "ValidateCheckpoint";
    case Boundary::InspectCheckpoint: return "InspectCheckpoint";
    case Boundary::PublishCheckpoint: return "PublishCheckpoint";
  }
  return "UnknownBoundary";
}

// Real filesystem operations with deterministic interruption at engine boundaries.
class InterruptingEngine : public TransferEngine {
 public:
  using Callback = std::function<void(Boundary, Cancellation)>;
  InterruptingEngine(const Path& storage, Callback callback) : posix_(storage), callback_(std::move(callback)) {}
  IoEngine type() const override { return IoEngine::POSIX_COPY; }
  PreparedRestore PrepareRestore(const StorageBackend& source, Cancellation cancellation,
      const PublishedArtifact* artifact, bool metadata_only, TransferMode mode) const override
  {
    callback_(Boundary::PrepareRestore, cancellation);
    return posix_.PrepareRestore(source, cancellation, artifact, metadata_only, mode);
  }
  RestoreAccess StageRestore(PreparedRestore preparation, const Path& destination, Cancellation cancellation) const override
  {
    auto access = posix_.StageRestore(std::move(preparation), destination, cancellation);
    callback_(Boundary::StageRestore, cancellation);
    return access;
  }
  Path PrepareCheckpoint(const StorageBackend& destination, const Path& staging_directory,
      const std::string& transaction_id, TransferMode mode, const PublishedArtifact* artifact,
      Cancellation cancellation) const override
  {
    ValidateCheckpointDestination(destination, artifact, cancellation);
    return posix_.PrepareCheckpoint(destination, staging_directory, transaction_id, mode, artifact, cancellation);
  }
  void ValidateCheckpointDestination(const StorageBackend& destination,
      const PublishedArtifact* artifact, Cancellation cancellation) const override
  {
    callback_(Boundary::ValidateCheckpoint, cancellation);
    posix_.ValidateCheckpointDestination(destination, artifact, cancellation);
  }
  CheckpointInventory InspectCheckpoint(const Path& source, const PublishedArtifact* artifact,
      Cancellation cancellation) const override
  {
    callback_(Boundary::InspectCheckpoint, cancellation);
    return posix_.InspectCheckpoint(source, artifact, cancellation);
  }
  void PublishCheckpoint(const Path& source, const StorageBackend& destination, CheckpointInventory inventory,
      CheckpointPublication* publication, Cancellation cancellation, TransferMode mode) const override
  {
    callback_(Boundary::PublishCheckpoint, cancellation);
    posix_.PublishCheckpoint(source, destination, std::move(inventory), publication, cancellation, mode);
  }

 private:
  PosixCopyEngine posix_;
  Callback callback_;
};

// Installs the current thread's directory-creation callback for this scope.
class ScopedMkdirHook {
 public:
  explicit ScopedMkdirHook(std::function<void(const char*)> hook);
  ~ScopedMkdirHook();
};
}  // namespace snapshot::pagebroker::test

namespace snapshot::pagebroker {
namespace fs = std::filesystem;

class BrokerTransferTest : public ::testing::Test, public test::RequestBuilder {
 protected:
  void SetUp() override
  {
    fs::create_directories(source_);
    std::ofstream(source_ / "image") << "image";
  }

  void Intercept(Broker& broker, test::InterruptingEngine::Callback callback)
  {
    broker.io_engines_.clear();
    broker.io_engines_.push_back(std::make_unique<test::InterruptingEngine>(storage_, std::move(callback)));
  }

  Request Restore()
  {
    auto request = RequestFor("restore");
    test::Configure(request.mutable_staged_restore()->mutable_source(), request.mutable_staged_restore()->mutable_io_engine(),
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
    test::Configure(destination, engine, storage_ / "published");
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

  Response ExecuteGpu(Broker& broker, const Transaction::GpuOperationPtr& operation)
  {
    std::mutex mutex;
    return broker.ExecuteGpu(RequestFor("gpu"), operation, std::unique_lock(mutex));
  }

  std::shared_ptr<Transaction> SeedRestore(Broker& broker, Transaction::State state)
  {
    auto transaction = broker.CreateOrGetTransaction("seed");
    std::lock_guard lock(transaction->mutex());
    transaction->PrepareTransfer(broker.resources_, 0, nullptr, broker.lifetime_);
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

}  // namespace snapshot::pagebroker

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <chrono>
#include <deque>
#include <exception>
#include <functional>
#include <memory>
#include <mutex>
#include <system_error>
#include <optional>
#include <unordered_map>
#include <vector>

#include "checkpoint_transaction_descriptor.hpp"
#include "pagebroker_types.hpp"
#include "restore_transaction_descriptor.hpp"
#include "transaction.hpp"
#include "transaction_defaults.hpp"
#include "s3_config.hpp"
#include "model_streamer_config.hpp"
#include "transfer/engine/transfer_engine.hpp"

namespace snapshot::pagebroker {
class Broker {
 public:
  Broker(Path staging_root, Path storage_root, gpu::GpuEnginePtr gpu_engine = nullptr,
      std::optional<S3Config> config = std::nullopt,
      std::chrono::seconds lifetime = kDefaultTransactionLifetime, ModelStreamerOptions model_streamer = {});
  ~Broker();
  Response HandleGpuRequest(const Request& request, CancellationPtr cancellation,
                            std::vector<FileDescriptor> target_descriptors);
  Response HandleRequest(const Request& request);
  void ReapExpiredTransactions(std::chrono::steady_clock::time_point now);
  using Clock = std::function<std::chrono::steady_clock::time_point()>;
  void SetClock(Clock clock);
  // Stops admission and signals transfer/GPU cancellation without taking transaction
  // mutexes or waiting for work to finish. Safe to call repeatedly; callers drain handlers.
  void Shutdown();

 private:
  friend class BrokerTransferTest;
  using Engines = std::vector<std::unique_ptr<TransferEngine>>;
  using TransactionHandle = std::shared_ptr<Transaction>;
  using Transactions = std::unordered_map<std::string, TransactionHandle>;
  struct TerminalTransaction {
    std::string id;
    TransactionHandle transaction;
    std::chrono::steady_clock::time_point completed;
  };

  const TransferEngine& Engine(IoEngine engine_type) const;
  const TransferEngine& Engine(const IOEngine& engine) const;
  const TransferEngine& ArtifactEngine(const IOEngine* engine = nullptr) const;
  Response Metadata(const Request& request);
  TransactionHandle CreateOrGetTransaction(const std::string& transaction_id);
  TransactionHandle FindTransaction(const std::string& transaction_id);
  std::vector<std::pair<std::string, TransactionHandle>> TransactionSnapshot();
  // Attempts retention without waiting for the transaction mutex. Caller must not hold it.
  void RetainTerminalTransaction(const std::string& transaction_id);
  // Caller must hold transaction->mutex().
  void RetainTerminalTransactionLocked(const std::string& id, const TransactionHandle& transaction);
  void ReapTerminalTransactions(std::chrono::steady_clock::time_point now);
  bool ReserveStaging(uintmax_t bytes);
  void ReleaseStaging(uintmax_t bytes);
  Response AbortStaging(
      const Request& request, Transaction& transaction, const Path& staging_directory, const std::exception& error);
  Response PrepareDirectCheckpoint(const Request& request);
  Response ExecuteGpu(const Request& request, const Transaction::GpuOperationPtr& operation,
                      std::unique_lock<std::mutex> lock);
  Response Restore(const Request& request);
  Response DirectRestore(const Request& request);
  Response StageRestore(const Request& request, const StorageBackend& source, const TransferEngine& engine,
      const PublishedArtifact* artifact = nullptr);
  Response PrepareCheckpoint(const Request& request);
  Response StageCheckpoint(const Request& request, const StorageBackend& destination, const TransferEngine& engine,
      const PublishedArtifact* artifact = nullptr);
  // The Snapshot Agent sends COMMIT after CRIU returns; the provider will send it directly later.
  Response Commit(const Request& request);
  Response CleanupRestore(
      const Request& request, Transaction& transaction, const RestoreTransactionDescriptor& descriptor);
  Response PublishCheckpoint(
      const Request& request, Transaction& transaction, const CheckpointTransactionDescriptor& descriptor);
  Response Abort(const Request& request);
  std::chrono::seconds lifetime_;
  Path staging_root_;
  Engines io_engines_;
  gpu::GpuEnginePtr gpu_engine_;
  std::shared_ptr<TransactionResources> resources_;
  std::mutex transactions_mutex_;
  Transactions transactions_;
  bool stopping_ = false;
  std::mutex terminal_transactions_mutex_;
  std::deque<TerminalTransaction> terminal_transactions_;
  uintmax_t reserved_staging_bytes_ = 0;
  Clock now_ = [] { return std::chrono::steady_clock::now(); };
};
}  // namespace snapshot::pagebroker

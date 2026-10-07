// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <chrono>
#include <deque>
#include <exception>
#include <memory>
#include <mutex>
#include <system_error>
#include <optional>
#include <string>
#include <unordered_map>
#include <vector>

#include "artifact_store.hpp"
#include "checkpoint_transaction_descriptor.hpp"
#include "pagebroker_types.hpp"
#include "restore_transaction_descriptor.hpp"
#include "transaction.hpp"
#include "transfer_engine.hpp"

namespace snapshot::pagebroker {
class Broker {
 public:
  // store_id is this installation's configured store (empty means no store
  // is configured, e.g. an older chart release that does not yet pass
  // --storage-config): every artifact-addressed request fails INVALID_REQUEST
  // instead of being silently accepted against no store.
  Broker(Path staging_root, Path storage_root, gpu::GpuEnginePtr gpu_engine = nullptr, std::string store_id = "");
  ~Broker();
  Response HandleGpuRequest(const Request& request, CancellationPtr cancellation,
                            std::vector<FileDescriptor> target_descriptors);
  void CancelGpuWork();
  Response HandleRequest(const Request& request);
  void ReapExpiredTransactions(std::chrono::steady_clock::time_point now);

 private:
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
  TransactionHandle CreateOrGetTransaction(const std::string& transaction_id);
  TransactionHandle FindTransaction(const std::string& transaction_id);
  std::vector<std::pair<std::string, TransactionHandle>> TransactionSnapshot();
  std::error_code CleanupTransactionDirectory(const std::string& id, Transaction& transaction);
  void RetainTerminalTransaction(const std::string& transaction_id);
  void ReapTerminalTransactions();
  bool ReserveStaging(uintmax_t bytes);
  void ReleaseStaging(uintmax_t bytes);
  Response AbortStaging(
      const Request& request, Transaction& transaction, const Path& staging_directory, const std::exception& error);
  Response PrepareDirectCheckpoint(const Request& request);
  Response ExecuteGpu(const Request& request, const Transaction::GpuOperationPtr& operation,
                      std::unique_lock<std::mutex> lock);
  Response Restore(const Request& request);
  Response DirectRestore(const Request& request);
  Response StageRestore(const Request& request, const StorageBackend& source, const TransferEngine& engine);
  Response StageArtifactRestore(const Request& request, const RestorePlan& plan);
  Response PrepareCheckpoint(const Request& request);
  Response StageCheckpoint(const Request& request, const StorageBackend& destination, const TransferEngine& engine);
  Response StageArtifactCheckpoint(const Request& request, const ArtifactTarget& target);
  Response GetArtifactMetadata(const Request& request);
  // The Snapshot Agent sends COMMIT after CRIU returns; the provider will send it directly later.
  Response Commit(const Request& request);
  Response CleanupRestore(
      const Request& request, Transaction& transaction, const RestoreTransactionDescriptor& descriptor);
  Response PublishCheckpoint(
      const Request& request, Transaction& transaction, const CheckpointTransactionDescriptor& descriptor);
  Response PublishArtifactCheckpoint(
      const Request& request, Transaction& transaction, const CheckpointTransactionDescriptor& descriptor);
  // Throws ArtifactError(INVALID_REQUEST) when no store is configured.
  const PVCArtifactStore& RequireArtifactStore() const;
  Response Abort(const Request& request);
  Path staging_root_;
  Engines io_engines_;
  gpu::GpuEnginePtr gpu_engine_;
  std::optional<PVCArtifactStore> artifact_store_;
  std::mutex transactions_mutex_;
  Transactions transactions_;
  std::mutex terminal_transactions_mutex_;
  std::deque<TerminalTransaction> terminal_transactions_;
  uintmax_t reserved_staging_bytes_ = 0;
};
}  // namespace snapshot::pagebroker

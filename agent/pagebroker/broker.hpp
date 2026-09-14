// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <atomic>
#include <chrono>
#include <deque>
#include <exception>
#include <functional>
#include <memory>
#include <mutex>
#include <optional>
#include <system_error>
#include <unordered_map>
#include <vector>

#include "checkpoint_transaction_descriptor.hpp"
#include "cuda_engine.hpp"
#include "pagebroker_types.hpp"
#include "restore_transaction_descriptor.hpp"
#include "stage_ready_gate.hpp"
#include "transaction.hpp"
#include "transfer_engine.hpp"

namespace snapshot::pagebroker {
class Broker {
 public:
  Broker(
      Path staging_root,
      Path storage_root,
      uintmax_t max_staging_bytes,
      std::unique_ptr<CudaEngine> cuda_engine = nullptr,
      std::unique_ptr<TransferEngine> transfer_engine = nullptr,
      std::optional<size_t> direct_restore_descriptor_budget = std::nullopt,
      StageReadyGate::FailureForTesting stage_ready_failure_for_testing = {});
  Response HandleRequest(const Request& request);
  void ReapExpiredTransactions(std::chrono::steady_clock::time_point now);
  bool ReapRetainedStageReadyMarkers(
      std::chrono::system_clock::time_point now, std::string* error);
  bool ReapExitedCuda(std::string* error);
  bool BeginShutdownCuda(std::string* error);
  bool ShutdownCuda(std::string* error);
  bool ShutdownRequested() const { return shutdown_requested_.load(std::memory_order_acquire); }

 private:
  using Engines = std::vector<std::unique_ptr<TransferEngine>>;
  using TransactionHandle = std::shared_ptr<Transaction>;
  using Transactions = std::unordered_map<std::string, TransactionHandle>;
  struct TerminalTransaction {
    std::string id;
    TransactionHandle transaction;
    std::chrono::steady_clock::time_point completed;
  };

  const TransferEngine& Engine(TransferEngineType engine_type) const;
  const TransferEngine& Engine(const IOEngine& engine) const;
  TransactionHandle CreateOrGetTransaction(const std::string& transaction_id);
  TransactionHandle FindTransaction(const std::string& transaction_id);
  void RetainTerminalTransaction(const std::string& transaction_id);
  void ReapTerminalTransactions();
  bool ReserveStaging(uintmax_t bytes, bool copy_pending);
  void FinishStagingCopy(uintmax_t bytes);
  void ReleaseStaging(Transaction& transaction);
  bool ReserveDirectRestoreDescriptors(size_t descriptors);
  void ReleaseDirectRestoreDescriptors(Transaction& transaction);
  bool CleanupStaging(Transaction& transaction, const Path& staging_directory, std::error_code& error);
  Response AbortStaging(
      const Request& request, Transaction& transaction,
      const Path& staging_directory, const std::exception& error,
      Failure::Code code = Failure::STORAGE_ERROR);
  Response Restore(const Request& request);
  Response ReferenceRegularRestore(const Request& request);
  Response DirectRestore(const Request& request);
  Response StageRestore(const Request& request, const StorageBackend& source, const TransferEngine& engine);
  Response PrepareCheckpoint(const Request& request);
  Response StageCheckpoint(const Request& request, const StorageBackend& destination, const TransferEngine& engine);
  // The Snapshot Agent sends COMMIT after CRIU returns; the provider will send it directly later.
  Response Commit(const Request& request);
  Response CleanupRestore(
      const Request& request, Transaction& transaction, const RestoreTransactionDescriptor& descriptor);
  Response PublishCheckpoint(
      const Request& request, Transaction& transaction, const CheckpointTransactionDescriptor& descriptor);
  Response Abort(const Request& request);
  Response BeginRestore(const Request& request);
  Response ActivateRestore(const Request& request);
  Response CudaCheckpoint(const Request& request);
  Response BeginCheckpoint(const Request& request);
  Response CudaRestore(const Request& request);
  Response ExecuteCudaCheckpoint(const Request& request);
  Response ExecuteCudaRestore(const Request& request);
  Path staging_root_;
  StageReadyGate stage_ready_gate_;
  uintmax_t max_staging_bytes_;
  Engines io_engines_;
  std::unique_ptr<CudaEngine> cuda_engine_;
  std::atomic<bool> shutdown_requested_{false};
  std::mutex transactions_mutex_;
  Transactions transactions_;
  std::mutex terminal_transactions_mutex_;
  std::deque<TerminalTransaction> terminal_transactions_;
  std::mutex staging_capacity_mutex_;
  uintmax_t reserved_staging_bytes_ = 0;
  uintmax_t pending_copy_bytes_ = 0;
  std::mutex direct_restore_descriptor_mutex_;
  size_t direct_restore_descriptor_budget_ = 0;
  size_t reserved_direct_restore_descriptors_ = 0;
};
}  // namespace snapshot::pagebroker

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <thread>

#include "pagebroker_types.hpp"
#include "transfer_engine.hpp"

struct criu_provider_dump_session;
struct criu_provider_plan;

namespace snapshot::pagebroker {
class CheckpointTransactionDescriptor {
 public:
  CheckpointTransactionDescriptor(
      Path staging_directory, StorageBackend destination_storage, TransferEngineType engine_type);
  ~CheckpointTransactionDescriptor();
  CheckpointTransactionDescriptor(const CheckpointTransactionDescriptor&) = delete;
  CheckpointTransactionDescriptor& operator=(const CheckpointTransactionDescriptor&) = delete;
  CheckpointTransactionDescriptor(CheckpointTransactionDescriptor&& other) noexcept;
  CheckpointTransactionDescriptor& operator=(CheckpointTransactionDescriptor&& other) noexcept;

	void StartProvider();
	int TakeProviderSocket();
  const Path& staging_directory() const;
  const StorageBackend& destination_storage() const;
  TransferEngineType engine_type() const;

 private:
  Path staging_directory_;
  StorageBackend destination_storage_;
  TransferEngineType engine_type_;
	criu_provider_plan* dump_plan_ = nullptr;
	criu_provider_dump_session* dump_session_ = nullptr;
	int client_socket_ = -1;
	int server_socket_ = -1;
	std::thread server_;
	static int OpenOutput(void* context, const char* image, int flags);
	static int FinishDump(void* context, const criu_provider_plan* plan);
	static void AbortDump(void* context);
	void Close();
};
}  // namespace snapshot::pagebroker

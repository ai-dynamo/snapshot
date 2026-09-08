// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <thread>
#include <future>
#include <functional>

#include "transfer_engine.hpp"

struct criu_provider_plan;
struct criu_provider_session;

namespace snapshot::pagebroker {
class DirectRestoreTransactionDescriptor {
 public:
  DirectRestoreTransactionDescriptor(Path staging_directory, uintmax_t reserved_staging_bytes);
  ~DirectRestoreTransactionDescriptor();
  DirectRestoreTransactionDescriptor(const DirectRestoreTransactionDescriptor&) = delete;
  DirectRestoreTransactionDescriptor& operator=(const DirectRestoreTransactionDescriptor&) = delete;
  DirectRestoreTransactionDescriptor(DirectRestoreTransactionDescriptor&& other) noexcept;
  DirectRestoreTransactionDescriptor& operator=(DirectRestoreTransactionDescriptor&& other) noexcept;

  void Start(std::function<void(const Path&)> stage);
  void Wait();
  const Path& staging_directory() const;
  uintmax_t TakeReservedStagingBytes();
  int TakeClientSocket();

 private:
  static int ReadRange(void* context, const char* image, uint64_t offset, void* buffer, size_t length);
  static int OpenReadyImage(void* context, const char* image, int flags);
  void Close();
  Path staging_directory_;
  uintmax_t reserved_staging_bytes_ = 0;
  criu_provider_plan* plan_ = nullptr;
  criu_provider_session* session_ = nullptr;
  int client_socket_ = -1;
  int server_socket_ = -1;
  std::future<void> preparation_;
  std::thread server_;
  std::string s3_prefix_;
};
}  // namespace snapshot::pagebroker

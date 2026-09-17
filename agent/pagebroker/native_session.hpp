// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0
#pragma once

#include "transaction.hpp"
#include "v1/pagebroker.pb.h"

namespace snapshot::pagebroker {
// A transaction-scoped native CUDA worker. Its PID namespace and storage root
// are pinned at admission, before CRIU replaces the placeholder process tree.
class NativeSession {
 public:
  NativeSession(std::shared_ptr<Transaction>, const v1::BindNativeSession&, const Path&);
  ~NativeSession();
  v1::NativeSessionReply Execute(const v1::NativeSessionRequest&);
 private:
  void Start(uint32_t target_pid);
  void Stop() noexcept;
  std::shared_ptr<Transaction> transaction_;
  v1::BindNativeSession binding_;
  Path executable_;
  FileDescriptor namespace_fd_{-1}, directory_fd_{-1}, connection_{-1};
  pid_t worker_ = -1;
  bool admitted_ = false, finished_ = false;
  v1::NativeSessionRequest::Operation phase_ = v1::NativeSessionRequest::UNSPECIFIED;
};
}  // namespace snapshot::pagebroker

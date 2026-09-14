// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// All rights reserved. SPDX-License-Identifier: Apache-2.0

#pragma once

#include <chrono>
#include <string>

#include "daemon_protocol.h"

namespace snapshot::pagebroker {

enum class CudaWorkerRpcStatus {
  kOk,
  kInvalidRequest,
  kConnectFailed,
  kTimeout,
  kDisconnected,
  kProtocolError,
  kFatalResponse,
  kPoolFailStopped,
};

struct CudaWorkerRpcResult {
  CudaWorkerRpcStatus status = CudaWorkerRpcStatus::kProtocolError;
  cuda_checkpoint_daemon::Response response;
  std::string error;
  // Once a mutating request has been sent, an EOF, timeout, malformed reply,
  // or fatal reply is an unknown/unsafe outcome and must never be replayed.
  bool unknown_outcome = false;

  explicit operator bool() const { return status == CudaWorkerRpcStatus::kOk; }
};

class CudaWorkerClient {
public:
  CudaWorkerClient(std::string socket_path,
                   std::chrono::milliseconds operation_timeout,
                   std::chrono::milliseconds health_timeout);

  CudaWorkerRpcResult
  Call(const cuda_checkpoint_daemon::Request &request) const;
  CudaWorkerRpcResult Health() const;

  const std::string &socket_path() const { return socket_path_; }

private:
  CudaWorkerRpcResult
  CallSocket(const std::string &path,
             const cuda_checkpoint_daemon::Request &request,
             std::chrono::milliseconds timeout) const;

  std::string socket_path_;
  std::chrono::milliseconds operation_timeout_;
  std::chrono::milliseconds health_timeout_;
};

} // namespace snapshot::pagebroker

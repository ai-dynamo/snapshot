// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0
#pragma once

#include "gpu_engine.pb.h"
#include "transaction.hpp"
#include "v1/pagebroker.pb.h"

namespace snapshot::pagebroker {
class GpuEngine;
// A transaction-scoped session in the GPU engine. PID namespace and storage root
// are pinned at admission, before CRIU replaces the placeholder process tree.
// LOAD admission also reads and size-checks the participant manifest then, while
// storage is idle; the engine receives it instead of re-reading it in PREPARE.
class NativeSession {
 public:
  NativeSession(std::shared_ptr<Transaction>, const v1::BindNativeSession&, std::shared_ptr<GpuEngine>);
  ~NativeSession();
  v1::NativeSessionReply Execute(const v1::NativeSessionRequest&);
 private:
  void Start(uint32_t target_pid);
  void Stop() noexcept;
  std::shared_ptr<Transaction> transaction_;
  v1::BindNativeSession binding_;
  std::shared_ptr<GpuEngine> engine_;
  FileDescriptor namespace_fd_{-1}, directory_fd_{-1}, connection_{-1};
  internal::LoadManifest load_manifest_;
  bool finished_ = false;
};
}  // namespace snapshot::pagebroker

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0
#pragma once

#include "file_descriptor.hpp"
#include "v1/pagebroker.pb.h"
#include <filesystem>
#include <mutex>

namespace snapshot::pagebroker {
// Owns the long-lived CUDA process without loading CUDA into the CPU broker.
// Start completes device/ring initialization before the daemon accepts work.
class GpuEngine {
 public:
  explicit GpuEngine(std::filesystem::path executable);
  ~GpuEngine();
  void Start();
  FileDescriptor Bind(const v1::BindNativeSession&, int host_pid, int directory);
  // Transport failure is an engine-wide fault. Reap before releasing storage
  // admission: a disconnected session socket alone does not prove DMA drained.
  void Stop() noexcept;
 private:
  std::filesystem::path executable_;
  std::mutex mutex_;
  FileDescriptor control_{-1};
  pid_t process_ = -1;
};
}  // namespace snapshot::pagebroker

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0
#pragma once

#include <cstddef>

namespace snapshot::cuda_checkpoint {
struct DaemonOptions {
  bool custom_storage = true;
  std::size_t buffer_count = 32;
  std::size_t chunk_bytes = 128ULL * 1024 * 1024;
  std::size_t max_pinned_bytes = 0;
};

// Run inside a dedicated GPU process. The control socket belongs to its parent;
// READY follows eager initialization, and owner loss or unsafe CUDA completion
// retires this process. A future PageBroker GPU process can use the same runner.
[[noreturn]] void RunDaemon(int control_fd, const DaemonOptions& options);
} // namespace snapshot::cuda_checkpoint

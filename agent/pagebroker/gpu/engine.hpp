// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0
#pragma once

#include <cstddef>
#include <cstdint>
#include <memory>
#include <string>
#include <vector>

#include "cancellation.hpp"
#include "errors.hpp"

namespace snapshot::pagebroker::gpu {
enum class Direction { Checkpoint, Restore };
struct Participant {
  uint32_t captured_pid;
  uint32_t target_pid;  // Host PID resolved by the agent.
};
struct DeviceMapping {
  std::string source_uuid;
  std::string target_uuid;
};
struct EngineOptions {
  size_t buffer_count = 32;
  size_t chunk_bytes = 128ULL * 1024 * 1024;
  size_t max_pinned_bytes = 0;
};
struct ParticipantResult {
  uint32_t captured_pid = 0;
  uint64_t bytes = 0;
};

// Construct when GPU execution starts. Retains the artifact directory and
// validates restore files. The broker keeps it until Commit or Abort finishes.
class Artifact {
 public:
  Artifact(int directory_fd, Direction direction,
           std::vector<uint32_t> captured_pids, std::vector<std::string> visible_devices,
           std::vector<DeviceMapping> device_map = {});
  ~Artifact();
  Artifact(const Artifact&) = delete;
  Artifact& operator=(const Artifact&) = delete;

 private:
  friend class GpuEngine;
  struct Impl;
  std::unique_ptr<Impl> impl_;
};

// Owns CustomStorage calls, contexts and transfer resources in this process.
// Calls can overlap for different artifacts. CUDA mappings stay in the engine.
class GpuEngine {
 public:
  explicit GpuEngine(EngineOptions options = {});
  ~GpuEngine();
  bool Available() const;
  std::vector<ParticipantResult> Checkpoint(Artifact&, const std::vector<Participant>&, Cancellation&);
  std::vector<ParticipantResult> Restore(Artifact&, const std::vector<Participant>&, Cancellation&);

 private:
  struct Impl;
  std::unique_ptr<Impl> impl_;
  std::vector<ParticipantResult> Execute(Artifact&, const std::vector<Participant>&, Cancellation&, Direction);
};
}  // namespace snapshot::pagebroker::gpu

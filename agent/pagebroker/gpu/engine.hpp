// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0
#pragma once

#include <cstddef>
#include <cstdint>
#include <memory>
#include <mutex>
#include <span>
#include <string>
#include <vector>

#include "cancellation.hpp"
#include "errors.hpp"

namespace snapshot::pagebroker::gpu {
// Wait for exclusive access to a device ring while honoring cancellation.
std::unique_lock<std::timed_mutex> AcquireDevice(std::timed_mutex& mutex, const Cancellation& cancellation);

enum class Direction { Checkpoint, Restore };
struct Participant {
  uint32_t captured_pid;
  uint32_t target_pid;  // Host PID resolved by the agent.
  // Borrowed for this call. The driver duplicates the supplied process handle.
  int pidfd = -1;
  bool operator==(const Participant& other) const
  {
    return captured_pid == other.captured_pid && target_pid == other.target_pid;
  }
};
struct DeviceMapping {
  std::string source_uuid;
  std::string target_uuid;
  bool operator==(const DeviceMapping&) const = default;
};
// Validate the complete participant set before any CUDA operation starts.
void ValidateParticipants(std::span<const uint32_t> captured_pids, std::span<const Participant> participants);

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
  struct ArtifactState;
  std::unique_ptr<ArtifactState> state_;
};

// Owns CustomStorage calls, contexts and transfer resources in this process.
// Calls can overlap for different artifacts. CUDA mappings stay in the engine.
// Returns and exceptions both confirm that transfers drained. Unsafe cleanup
// exits the process before releasing resources that CUDA or storage may use.
class GpuEngine {
 public:
  explicit GpuEngine(EngineOptions options = {});
  ~GpuEngine();
  bool Available() const;
  std::vector<ParticipantResult> Checkpoint(Artifact&, const std::vector<Participant>&, Cancellation&);
  std::vector<ParticipantResult> Restore(Artifact&, const std::vector<Participant>&, Cancellation&);

 private:
  struct GpuEngineState;
  class Batch;
  std::unique_ptr<GpuEngineState> state_;
};
using ArtifactPtr = std::unique_ptr<Artifact>;
using GpuEnginePtr = std::shared_ptr<GpuEngine>;

}  // namespace snapshot::pagebroker::gpu

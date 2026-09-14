// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// All rights reserved. SPDX-License-Identifier: Apache-2.0

#pragma once

#include <atomic>
#include <chrono>
#include <cstdint>
#include <filesystem>
#include <string>
#include <vector>

namespace snapshot::pagebroker {

struct CuinterposeTarget {
  uint32_t host_pid;
  uint32_t namespace_pid;
};

struct CuinterposeResult {
  bool succeeded = false;
  // True once the coordinator process was started. Its failure then has an
  // unknown workload outcome and must be handled fail-closed.
  bool dispatched = false;
  std::string error;
};

struct CuinterposeStateMetadata {
  uint32_t protocol_version = 0;
  uint64_t size_bytes = 0;
  std::string sha256;
  uint32_t participant_count = 0;
};

class CuinterposeCoordinator {
public:
  CuinterposeCoordinator(std::filesystem::path binary,
                         std::filesystem::path process_root,
                         std::chrono::seconds timeout);
  virtual ~CuinterposeCoordinator() = default;

  // Enforce the all-or-none shim contract before a destructive operation.
  virtual bool ValidateEndpoints(const std::vector<CuinterposeTarget> &targets,
                                 bool expected, std::string *error) const;
  virtual bool ReadState(const std::filesystem::path &staging_directory,
                         uint32_t participant_count,
                         CuinterposeStateMetadata *metadata,
                         std::string *error) const;
  // Parse and validate the complete saved topology in the canonical
  // coordinator before any native CUDA restore mutates the destination.
  virtual CuinterposeResult ValidateState(
      const std::filesystem::path &staging_directory,
      const std::atomic<bool> &shutting_down) const;
  virtual CuinterposeResult Prepare(
      const std::vector<CuinterposeTarget> &targets,
      const std::filesystem::path &staging_directory,
      const std::atomic<bool> &shutting_down,
      bool reject_legacy_ipc) const;
  virtual CuinterposeResult Restore(
      const std::vector<CuinterposeTarget> &targets,
      const std::filesystem::path &staging_directory,
      const std::atomic<bool> &shutting_down) const;

private:
  CuinterposeResult Run(const char *operation,
                        const std::vector<CuinterposeTarget> &targets,
                        const std::filesystem::path &staging_directory,
                        const std::atomic<bool> &shutting_down) const;

  std::filesystem::path binary_;
  std::filesystem::path process_root_;
  std::chrono::seconds timeout_;
};

} // namespace snapshot::pagebroker

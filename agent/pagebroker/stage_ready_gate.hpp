// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <chrono>
#include <cstddef>
#include <functional>
#include <filesystem>
#include <mutex>
#include <stdexcept>
#include <string>
#include <string_view>

#include "file_descriptor.hpp"

namespace snapshot::pagebroker {

enum class StageReadyBackend {
  REGULAR,
  POSIX_CUSTOM_STORAGE,
};

struct StageReadyIdentity {
  std::string node_name;
  std::string pod_uid;
  std::string destination_container;
  std::string content_uid;
  std::string source_container;
  std::string container_id;
  std::string transaction_id;
  std::string stage_request_id;
  StageReadyBackend backend;
};

enum class StageReadyState {
  READY,
  COMMITTED,
  ABORTED,
  EXPIRED,
  COORDINATOR_LOST,
};

struct StageReadyHandle {
  std::string operation_id;
  std::string transaction_id;
  std::string stage_request_id;
  std::string pagebroker_generation;
};

class StageReadyDurabilityUncertain : public std::runtime_error {
 public:
  StageReadyDurabilityUncertain(StageReadyHandle handle,
                                StageReadyState state,
                                std::string message);

  const StageReadyHandle& handle() const { return handle_; }
  StageReadyState state() const { return state_; }

 private:
  StageReadyHandle handle_;
  StageReadyState state_;
};

// Owns PageBroker's durable, owner-scoped GMS restore ordering records. The
// filesystem checks harden links, ownership drift, and replacement races; they
// do not authenticate PageBroker against same-UID writers on the storage root,
// which are part of the deployment's trusted computing base. The broker
// transaction lifecycle is intentionally not coupled to this module; callers
// publish and transition a handle at their existing durable boundaries.
class StageReadyGate {
 public:
  using FailureForTesting = std::function<int(const std::string&)>;

  StageReadyGate(std::filesystem::path storage_root,
                 std::string owner_identity,
                 std::string node_name,
                 FailureForTesting failure_for_testing = {});

  StageReadyHandle PublishReady(const StageReadyIdentity& identity);
  void Transition(const StageReadyHandle& handle, StageReadyState state);
  void BeginShutdown();
  size_t ReapRetainedMarkers(
      std::chrono::system_clock::time_point now,
      std::chrono::system_clock::duration retention);

  const std::string& owner_id() const { return owner_id_; }
  const std::string& generation() const { return generation_; }
  const std::string& node_name() const { return node_name_; }

  static std::string OperationId(std::string_view pod_uid,
                                 std::string_view destination_container);

 private:
  struct Marker;

  void RecoverPriorGeneration();
  Marker ReadMarker(const std::string& operation_id) const;
  void WriteMarker(const std::string& operation_id,
                   const Marker& marker,
                   bool no_replace,
                   const char* operation);
  void MaybeFail(const char* operation) const;

  std::filesystem::path storage_root_;
  std::filesystem::path owner_root_;
  FileDescriptor storage_root_fd_{-1};
  FileDescriptor owners_root_fd_{-1};
  FileDescriptor owner_root_fd_{-1};
  FileDescriptor markers_root_fd_{-1};
  FileDescriptor owner_lock_fd_{-1};
  std::string owner_id_;
  std::string node_name_;
  std::string generation_;
  FailureForTesting failure_for_testing_;
  mutable std::mutex mutex_;
  bool shutting_down_ = false;
  // An unlink that was visible before its directory fsync failed still needs
  // a durability retry even though the marker is absent on the next pass.
  bool reap_sync_pending_ = false;
};

}  // namespace snapshot::pagebroker

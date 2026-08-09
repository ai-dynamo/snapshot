// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
#pragma once

#include <cstdint>
#include <condition_variable>
#include <filesystem>
#include <map>
#include <memory>
#include <mutex>
#include <string>
#include <utility>
#include <vector>

namespace pagebroker {

class CopyPool;

struct ImageSpec {
  std::string name;
  std::string uri;
  std::uint64_t size{};
};

struct SourceRange {
  std::string object;
  std::uint64_t source_offset{};
  std::uint64_t dst_offset{};
  std::uint64_t length{};
};

struct HostMemoryObject {
  std::uint64_t memory_id{};
  std::string name;
  std::uint32_t pid{};
  std::uint32_t vma_id{};
  std::uint64_t shmid{};
  std::uint64_t dst_addr{};
  std::uint64_t length{};
  std::string semantics;
  std::string map_mode;
  std::vector<SourceRange> source_ranges;
};

struct Request {
  enum class Operation : std::uint32_t {
    Submit = 1,
    WaitReady = 2,
    PrepareCheckpoint = 3,
    Commit = 4,
    Abort = 5
  };
  Operation operation{};
  std::string transaction_id;
  std::string checkpoint_path;
  std::string manifest_ref;
  std::uint64_t resident_bytes{};
  std::vector<ImageSpec> images;
  std::vector<HostMemoryObject> host_memory_objects;

  Request() = default;
  Request(Operation op, std::string id, std::filesystem::path checkpoint)
      : operation(op), transaction_id(std::move(id)),
        checkpoint_path(checkpoint.string()) {}
};

struct Response {
  bool ok{};
  std::string transaction_id;
  std::string staging_path;
  std::string scratch_path;
  std::string error;
};

struct StagingState {
  struct MaterializedObject {
    HostMemoryObject spec;
    int fd{-1};
    ~MaterializedObject();
  };
  std::mutex mutex;
  std::condition_variable changed;
  std::size_t remaining_tasks{};
  bool fill_started{};
  bool complete{};
  bool cancelled{};
  bool provider_running{};
  std::string error;
  std::filesystem::path checkpoint_root;
  std::string manifest_ref;
  std::map<std::string, ImageSpec> images;
  std::map<std::string, std::shared_ptr<MaterializedObject>> vmas;
  std::map<std::uint64_t, std::shared_ptr<MaterializedObject>> shared;
};

bool decode_request(const void *data, std::size_t size, Request &request,
                    std::string &error);
std::string encode_response(const Response &response);

class TransactionManager {
public:
  TransactionManager(std::filesystem::path staging_root,
                     std::filesystem::path scratch_root, std::uint64_t budget);
  ~TransactionManager();
  Response submit(const Request &request);
  Response wait_ready(const Request &request);
  Response prepare_checkpoint(const Request &request);
  Response commit(const Request &request);
  Response abort(const Request &request);
  std::shared_ptr<StagingState>
  staging_state(const std::string &transaction_id);
  void cleanup();

private:
  struct TransactionState {
    std::filesystem::path checkpoint;
    std::uint64_t staged_bytes{};
    bool promote{};
    std::shared_ptr<StagingState> staging;
  };
  static void stop_staging(TransactionState &state, bool cancel);
  std::filesystem::path staging_root_, scratch_root_;
  std::uint64_t budget_;
  bool defer_fill_{};
  std::unique_ptr<CopyPool> copy_pool_;
  std::map<std::string, TransactionState> transactions_;
  std::uint64_t staged_bytes_{};
  std::mutex mutex_;
};

int serve(const std::filesystem::path &socket_path,
          const std::filesystem::path &staging_root,
          const std::filesystem::path &scratch_root, std::uint64_t budget);
int serve_provider(const std::filesystem::path &root, int socket_fd,
                   int diagnostic_fd,
                   std::shared_ptr<StagingState> staging = {});
#ifdef PAGEBROKER_TEST
bool test_copy_pool_priority();
void test_set_fill_delay(unsigned milliseconds);
#endif
} // namespace pagebroker

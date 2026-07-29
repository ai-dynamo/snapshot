// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
#pragma once

#include <cstdint>
#include <filesystem>
#include <map>
#include <mutex>
#include <optional>
#include <string>

namespace pagebroker {

struct Request {
  enum class Operation : std::uint32_t {
    Submit = 1,
    WaitReady = 2,
    Commit = 3,
    Abort = 4
  };
  Operation operation{};
  std::string transaction_id;
  std::string checkpoint_path;
};

struct Response {
  bool ok{};
  std::string transaction_id;
  std::string staging_path;
  std::string scratch_path;
  std::string error;
};

bool decode_request(const void *data, std::size_t size, Request &request,
                    std::string &error);
std::string encode_response(const Response &response);

class TransactionManager {
public:
  TransactionManager(std::filesystem::path staging_root,
                     std::filesystem::path scratch_root, std::uint64_t budget);
  Response submit(const Request &request);
  Response wait_ready(const Request &request);
  Response commit(const Request &request);
  Response abort(const Request &request);
  void cleanup();

private:
  struct TransactionState {
    std::uint64_t staged_bytes{};
  };
  std::filesystem::path staging_root_, scratch_root_;
  std::uint64_t budget_;
  std::map<std::string, TransactionState> transactions_;
  std::uint64_t staged_bytes_{};
  std::mutex mutex_;
};

int serve(const std::filesystem::path &socket_path,
          const std::filesystem::path &staging_root,
          const std::filesystem::path &scratch_root, std::uint64_t budget);
} // namespace pagebroker

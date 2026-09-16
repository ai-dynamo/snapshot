// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0
#pragma once

#include <map>
#include <memory>
#include <set>

#include "allocation_transport.hpp"
#include "transaction.hpp"

namespace snapshot::pagebroker {
// One connection, participant, direction and worker. This deliberately exposes
// no general Broker request entry point after the trusted agent binds it.
class AllocationSession {
 public:
  AllocationSession(std::shared_ptr<Transaction> transaction, const v1::BindAllocationSession& binding,
                    const Path& worker);
  ~AllocationSession();
  AllocationSession(const AllocationSession&) = delete;
  AllocationSession& operator=(const AllocationSession&) = delete;
  v1::AllocationSessionReply Execute(const v1::AllocationSessionRequest& request,
                                    const std::vector<FileDescriptor>& descriptors);

 private:
  class Worker;
  std::unique_ptr<Worker> worker_;
  std::shared_ptr<Transaction> transaction_;
  v1::BindAllocationSession binding_;
  FileDescriptor directory_fd_{-1};
  std::map<std::string, v1::AllocationExtent> extents_;
  std::set<std::string> transferred_;
  bool admitted_ = false;
  bool finished_ = false;
  bool failed_ = false;
};
}  // namespace snapshot::pagebroker

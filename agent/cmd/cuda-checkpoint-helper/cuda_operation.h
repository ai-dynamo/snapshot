/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#pragma once

#include <cuda.h>

#include <chrono>
#include <memory>
#include <string>

#include "daemon_protocol.h"

namespace cuda_checkpoint_operation {

struct InitializationMetrics {
  double cuda_init_seconds = 0.0;
  int cuda_device_count = 0;
  double device_enumeration_seconds = 0.0;
  int cuda_driver_version = 0;
  bool custom_storage_driver_api_available = false;
  bool custom_storage_transfer_backend_available = false;
  bool custom_storage_available = false;
};

// Service owns the CUDA operation state that must survive individual daemon
// requests, including target-scoped primary-context references.
class Service {
public:
  explicit Service(std::chrono::seconds max_operation_duration);
  Service(const Service &) = delete;
  Service &operator=(const Service &) = delete;
  ~Service();

  bool Initialize(InitializationMetrics *metrics, std::string *error);
  cuda_checkpoint_daemon::Response
  Execute(const cuda_checkpoint_daemon::Request &request);
  CUresult ReapExited(const std::string &proc_root,
                      std::string *identity_error);
  CUresult TerminateRetainedTargets(const std::string &proc_root,
                                    std::string *identity_error);
  CUresult ReleaseAll();

private:
  class Impl;
  std::unique_ptr<Impl> impl_;
};

} // namespace cuda_checkpoint_operation

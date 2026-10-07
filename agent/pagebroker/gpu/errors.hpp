// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0
#pragma once

#include <cuda.h>
#include <stdexcept>
#include <string>

namespace snapshot::pagebroker::gpu {
inline std::string
CudaErrorMessage(CUresult result, const char* operation)
{
  const char* name = nullptr;
  cuGetErrorName(result, &name);
  return std::string(operation) + ": " + (name ? name : "unknown CUDA error");
}

inline void
CheckCuda(CUresult result, const char* operation)
{
  if (result != CUDA_SUCCESS) {
    throw std::runtime_error(CudaErrorMessage(result, operation));
  }
}

// No CUDA work started. The caller can retry after the target is released.
class TargetConflict : public std::runtime_error {
 public:
  using std::runtime_error::runtime_error;
};

// The daemon must exit after this error. Keep resources that storage I/O or CUDA
// may still use until the process exits.
class FatalError : public std::runtime_error {
 public:
  using std::runtime_error::runtime_error;
};
}  // namespace snapshot::pagebroker::gpu

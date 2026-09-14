// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include <charconv>
#include <iostream>
#include <limits>
#include <string_view>

#include "daemon.hpp"

namespace {
constexpr size_t kDefaultMaxConcurrentCudaRestores = 2;
constexpr size_t kMaximumMaxConcurrentCudaRestores = 1024;
constexpr size_t kDefaultCudaWorkerCount = 8;
constexpr size_t kDefaultCudaCustomStorageWorkerCount = 8;
constexpr size_t kMaximumCudaWorkerCount = 128;
constexpr uint64_t kDefaultCudaOperationTimeoutSeconds = 30 * 60;
constexpr std::string_view kUsage =
    "usage: pagebroker --probe-ready socket_path\n"
    "   or: pagebroker socket_path staging_directory storage_root "
    "--max-concurrent-requests max_concurrent_requests "
    "--max-staging-bytes max_staging_bytes [--cuda] "
    "[--cuda-custom-storage] [--max-concurrent-cuda-restores "
    "max_concurrent_cuda_restores] [--cuda-operation-timeout-seconds "
    "cuda_operation_timeout_seconds] [--cuda-worker-count "
    "cuda_worker_count] [--cuda-custom-storage-worker-count "
    "cuda_custom_storage_worker_count]";

template <typename T>
bool
ParsePositiveSize(std::string_view value, T& result, T maximum)
{
  const auto [end, error] = std::from_chars(value.data(), value.data() + value.size(), result);
  return error == std::errc{} && end == value.data() + value.size() &&
         result > 0 && result <= maximum;
}
}  // namespace

int
main(int argc, char** argv)
{
  if (argc == 3 && std::string_view(argv[1]) == "--probe-ready")
    return static_cast<int>(ProbeDaemonReady(argv[2]));

  size_t max_concurrent_requests;
  uintmax_t max_staging_bytes;
  size_t max_concurrent_cuda_restores = kDefaultMaxConcurrentCudaRestores;
  size_t cuda_worker_count = kDefaultCudaWorkerCount;
  size_t cuda_custom_storage_worker_count =
      kDefaultCudaCustomStorageWorkerCount;
  uint64_t cuda_operation_timeout_seconds =
      kDefaultCudaOperationTimeoutSeconds;
  bool cuda = false;
  bool cuda_custom_storage = false;
  bool configured_cuda_restores = false;
  bool configured_cuda_timeout = false;
  bool configured_cuda_worker_count = false;
  bool configured_cuda_custom_storage_worker_count = false;
  bool invalid_flag = argc < 8 || argc > 18;
  for (int index = 8; !invalid_flag && index < argc; ++index) {
    const std::string_view flag(argv[index]);
    if (flag == "--cuda" && !cuda) {
      // The regular backend owns a fixed prewarmed worker generation.
      cuda = true;
    } else if (flag == "--cuda-custom-storage" && !cuda_custom_storage) {
      // CustomStorage owns an independent worker generation so launch-job
      // scopes cannot race through process-global environment state.
      cuda_custom_storage = true;
    } else if (flag == "--max-concurrent-cuda-restores" &&
               !configured_cuda_restores && ++index < argc &&
               ParsePositiveSize(argv[index], max_concurrent_cuda_restores,
                                 kMaximumMaxConcurrentCudaRestores)) {
      configured_cuda_restores = true;
    } else if (flag == "--cuda-operation-timeout-seconds" &&
               !configured_cuda_timeout && ++index < argc &&
               ParsePositiveSize(argv[index], cuda_operation_timeout_seconds,
                                 std::numeric_limits<uint64_t>::max())) {
      configured_cuda_timeout = true;
    } else if (flag == "--cuda-worker-count" &&
               !configured_cuda_worker_count && ++index < argc &&
               ParsePositiveSize(argv[index], cuda_worker_count,
                                 kMaximumCudaWorkerCount)) {
      configured_cuda_worker_count = true;
    } else if (flag == "--cuda-custom-storage-worker-count" &&
               !configured_cuda_custom_storage_worker_count && ++index < argc &&
               ParsePositiveSize(argv[index], cuda_custom_storage_worker_count,
                                 kMaximumCudaWorkerCount)) {
      configured_cuda_custom_storage_worker_count = true;
    } else {
      invalid_flag = true;
    }
  }
  if (invalid_flag || std::string_view(argv[4]) != "--max-concurrent-requests" ||
      !ParsePositiveSize(argv[5], max_concurrent_requests,
                         std::numeric_limits<size_t>::max()) ||
      std::string_view(argv[6]) != "--max-staging-bytes" ||
      !ParsePositiveSize(argv[7], max_staging_bytes,
                         std::numeric_limits<uintmax_t>::max()) ||
      ((configured_cuda_restores || configured_cuda_timeout) &&
       !cuda && !cuda_custom_storage) ||
      (configured_cuda_worker_count && !cuda) ||
      (configured_cuda_custom_storage_worker_count && !cuda_custom_storage)) {
    std::cerr << kUsage << '\n';
    return static_cast<int>(ExitCode::INVALID_ARGUMENTS);
  }
  return static_cast<int>(RunDaemon(argv[1], argv[2], argv[3],
                                    max_concurrent_requests, max_staging_bytes, cuda,
                                    cuda_custom_storage,
                                    max_concurrent_cuda_restores,
                                    cuda_operation_timeout_seconds,
                                    cuda_worker_count,
                                    cuda_custom_storage_worker_count));
}

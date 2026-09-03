/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#include "transfer_scheduler.h"

#include <chrono>
#include <exception>
#include <string>
#include <thread>
#include <vector>

namespace cuda_checkpoint_transfer {
namespace {

using Clock = std::chrono::steady_clock;

double ElapsedSeconds(Clock::time_point start) {
  return std::chrono::duration<double>(Clock::now() - start).count();
}

} // namespace

bool TransferBatch(const std::vector<ScheduledTransfer> &jobs,
                   TransferOperation operation, const TransferOptions &options,
                   Clock::time_point deadline, TransferBatchResult *result) {
  if (result == nullptr) {
    return false;
  }
  const auto orchestration_start = Clock::now();
  result->metrics.assign(jobs.size(), {});
  result->error.clear();
  std::vector<std::thread> workers;
  std::vector<unsigned char> worker_success(jobs.size(), 0);
  std::vector<std::string> worker_errors(jobs.size());
  TransferCancellation cancellation(deadline);
  try {
    workers.reserve(jobs.size());
    for (size_t job_index = 0; job_index < jobs.size(); ++job_index) {
      workers.emplace_back([&, job_index] {
        const auto &job = jobs[job_index];
        try {
          const bool transferred = TransferExtent(
              job.device_ptr, job.extent_size, job.stream, job.context,
              job.storage, operation, options, &cancellation,
              &result->metrics[job_index], &worker_errors[job_index]);
          worker_success[job_index] = transferred;
          if (!transferred) {
            cancellation.Cancel();
          }
        } catch (const std::exception &exception) {
          cancellation.Cancel();
          worker_errors[job_index] = exception.what();
        } catch (...) {
          cancellation.Cancel();
          worker_errors[job_index] = "unknown worker exception";
        }
      });
    }
  } catch (const std::exception &exception) {
    cancellation.Cancel();
    result->error = "failed to start custom storage worker: " +
                    std::string(exception.what());
  } catch (...) {
    cancellation.Cancel();
    result->error =
        "failed to start custom storage worker: unknown thread creation exception";
  }
  for (auto &worker : workers) {
    worker.join();
  }
  result->orchestration_seconds = ElapsedSeconds(orchestration_start);
  if (!result->error.empty()) {
    return false;
  }
  for (size_t job_index = 0; job_index < jobs.size(); ++job_index) {
    if (!worker_success[job_index]) {
      result->error =
          "custom storage transfer failed for device index " +
          std::to_string(jobs[job_index].device_index) + ": " +
          worker_errors[job_index];
      return false;
    }
  }
  return true;
}

} // namespace cuda_checkpoint_transfer

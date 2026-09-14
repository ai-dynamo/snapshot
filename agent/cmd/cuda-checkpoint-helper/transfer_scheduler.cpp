/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#include "transfer_scheduler.h"

#include <algorithm>
#include <chrono>
#include <exception>
#include <limits>
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

bool TransferConcurrencyLimit(const TransferOptions &options, size_t *jobs,
                              size_t *peak_pinned_bytes,
                              std::string *error) {
  if (jobs == nullptr || peak_pinned_bytes == nullptr || error == nullptr ||
      !ValidateTransferOptions(options, error)) {
    return false;
  }
  if (options.buffer_count != 0 &&
      options.chunk_bytes >
          std::numeric_limits<size_t>::max() / options.buffer_count) {
    *error = "transfer buffer size calculation overflow";
    return false;
  }
  const size_t per_job_bytes = options.buffer_count * options.chunk_bytes;
  if (per_job_bytes == 0 || per_job_bytes > kMaximumPinnedBytesPerOperation) {
    *error = "one transfer extent exceeds the pinned-memory budget";
    return false;
  }
  *jobs = std::min(kMaximumConcurrentTransferJobs,
                   kMaximumPinnedBytesPerOperation / per_job_bytes);
  *peak_pinned_bytes = *jobs * per_job_bytes;
  return *jobs != 0;
}

bool TransferBatch(const std::vector<ScheduledTransfer> &jobs,
                   TransferOperation operation, const TransferOptions &options,
                   Clock::time_point deadline, TransferBatchResult *result,
                   TransferCancellation *external_cancellation) {
  if (result == nullptr) {
    return false;
  }
  const auto orchestration_start = Clock::now();
  result->peak_pinned_bytes = 0;
  result->maximum_concurrent_jobs = 0;
  result->waves = 0;
  size_t concurrency = 0;
  if (!TransferConcurrencyLimit(options, &concurrency,
                                &result->peak_pinned_bytes,
                                &result->error)) {
    result->metrics.clear();
    result->orchestration_seconds = ElapsedSeconds(orchestration_start);
    return false;
  }
  result->metrics.assign(jobs.size(), {});
  result->error.clear();
  result->maximum_concurrent_jobs = std::min(concurrency, jobs.size());
  result->peak_pinned_bytes = jobs.empty() ? 0 :
      result->maximum_concurrent_jobs * options.buffer_count * options.chunk_bytes;
  result->waves = jobs.empty() ? 0 :
      (jobs.size() + concurrency - 1) / concurrency;
  TransferCancellation local_cancellation(deadline);
  TransferCancellation *cancellation = external_cancellation == nullptr
                                           ? &local_cancellation
                                           : external_cancellation;
  if (cancellation->IsCancelled()) {
    result->error = "custom storage transfer cancelled";
    result->orchestration_seconds = ElapsedSeconds(orchestration_start);
    return false;
  }
  for (size_t wave_start = 0; wave_start < jobs.size();
       wave_start += concurrency) {
    const size_t wave_end = std::min(jobs.size(), wave_start + concurrency);
    std::vector<std::thread> workers;
    std::vector<unsigned char> worker_success(wave_end - wave_start, 0);
    std::vector<std::string> worker_errors(wave_end - wave_start);
    try {
      workers.reserve(wave_end - wave_start);
      for (size_t job_index = wave_start; job_index < wave_end; ++job_index) {
        workers.emplace_back([&, job_index] {
          const size_t wave_index = job_index - wave_start;
          const auto &job = jobs[job_index];
          try {
            const bool transferred = TransferExtent(
                job.device_ptr, job.extent_size, job.stream, job.context,
                job.storage, operation, options, cancellation,
                &result->metrics[job_index], &worker_errors[wave_index]);
            worker_success[wave_index] = transferred;
            if (!transferred)
              cancellation->Cancel();
          } catch (const std::exception &exception) {
            cancellation->Cancel();
            worker_errors[wave_index] = exception.what();
          } catch (...) {
            cancellation->Cancel();
            worker_errors[wave_index] = "unknown worker exception";
          }
        });
      }
    } catch (const std::exception &exception) {
      cancellation->Cancel();
      result->error = "failed to start custom storage worker: " +
                      std::string(exception.what());
    } catch (...) {
      cancellation->Cancel();
      result->error = "failed to start custom storage worker: unknown thread "
                      "creation exception";
    }
    for (auto &worker : workers)
      worker.join();
    if (!result->error.empty())
      break;
    for (size_t job_index = wave_start; job_index < wave_end; ++job_index) {
      const size_t wave_index = job_index - wave_start;
      if (!worker_success[wave_index]) {
        result->error =
            "custom storage transfer failed for device index " +
            std::to_string(jobs[job_index].device_index) + ": " +
            worker_errors[wave_index];
        break;
      }
    }
    if (!result->error.empty())
      break;
  }
  result->orchestration_seconds = ElapsedSeconds(orchestration_start);
  return result->error.empty();
}

} // namespace cuda_checkpoint_transfer

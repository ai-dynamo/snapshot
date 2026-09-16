// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "../cmd/cuda-checkpoint-helper/transfer_engine.hpp"
#include "../cmd/cuda-checkpoint-helper/content_digest.hpp"
#include "file_descriptor.hpp"

#include <fcntl.h>
#include <sys/stat.h>
#include <unistd.h>
#include <algorithm>
#include <chrono>
#include <cstdlib>
#include <cstring>
#include <limits>
#include <memory>

namespace cuda_checkpoint_transfer {
namespace {
using Clock = std::chrono::steady_clock;

double ElapsedSeconds(Clock::time_point start)
{
  return std::chrono::duration<double>(Clock::now() - start).count();
}

std::string CudaError(CUresult status)
{
  const char* name = nullptr;
  cuGetErrorName(status, &name);
  return name ? name : "unknown CUDA error";
}

class TransferSlot {
 public:
  ~TransferSlot()
  {
    // An unknown DMA outcome is handled by worker termination. Never free
    // memory underneath the GPU while unwinding a failed transfer.
    if (cuda_may_access_)
      return;
    if (event_)
      cuEventDestroy(event_);
    if (data_ && cuMemHostUnregister(data_) == CUDA_SUCCESS)
      free(data_);
  }

  CUresult Allocate(size_t size)
  {
    if (posix_memalign(&data_, kBufferAlignment, size))
      return CUDA_ERROR_OUT_OF_MEMORY;
    auto status = cuMemHostRegister(data_, size, 0);
    if (status != CUDA_SUCCESS) {
      free(data_);
      data_ = nullptr;
      return status;
    }
    return cuEventCreate(&event_, CU_EVENT_DISABLE_TIMING);
  }

  bool Wait(TransferMetrics* metrics, std::string* error)
  {
    if (!pending_)
      return true;
    auto start = Clock::now();
    auto status = cuEventSynchronize(event_);
    metrics->cuda_wait_seconds += ElapsedSeconds(start);
    if (status != CUDA_SUCCESS) {
      *error = "CUDA event synchronization: " + CudaError(status);
      return false;
    }
    Complete();
    return true;
  }

  bool Copy(TransferOperation operation, const TransferChunk& chunk, CUdeviceptr device,
            CUstream stream, std::string* error)
  {
    // Set before enqueue: failure does not establish that no DMA was posted.
    cuda_may_access_ = true;
    auto status = operation == TransferOperation::kCheckpoint
        ? cuMemcpyDtoHAsync(data_, device + chunk.logical_offset, chunk.size, stream)
        : cuMemcpyHtoDAsync(device + chunk.logical_offset, data_, chunk.size, stream);
    if (status == CUDA_SUCCESS)
      status = cuEventRecord(event_, stream);
    if (status != CUDA_SUCCESS) {
      *error = "CUDA asynchronous copy: " + CudaError(status);
      return false;
    }
    pending_ = true;
    return true;
  }

  void Complete() { pending_ = false; cuda_may_access_ = false; }
  void* data() const { return data_; }

  bool Close(std::string* error)
  {
    if (cuda_may_access_) {
      *error = "CUDA transfer buffer retained: completion unknown";
      return false;
    }
    bool success = true;
    if (event_) {
      success = cuEventDestroy(event_) == CUDA_SUCCESS;
      event_ = nullptr;
    }
    if (data_) {
      if (cuMemHostUnregister(data_) == CUDA_SUCCESS) {
        free(data_);
        data_ = nullptr;
      } else {
        success = false;
      }
    }
    if (!success)
      *error = "CUDA transfer buffer cleanup failed";
    return success;
  }

 private:
  void* data_ = nullptr;
  CUevent event_ = nullptr;
  bool pending_ = false;
  bool cuda_may_access_ = false;
};

class StreamDrainGuard {
 public:
  StreamDrainGuard(CUstream stream, std::vector<std::unique_ptr<TransferSlot>>& slots)
      : stream_(stream), slots_(slots) {}
  ~StreamDrainGuard()
  {
    if (armed_ && cuStreamSynchronize(stream_) == CUDA_SUCCESS)
      for (auto& slot : slots_) slot->Complete();
  }
  void Disarm() { armed_ = false; }
 private:
  CUstream stream_;
  std::vector<std::unique_ptr<TransferSlot>>& slots_;
  bool armed_ = true;
};

bool OpenStorageFiles(const StorageLayout& storage, std::vector<FileDescriptor>& files, std::string* error)
{
  for (const auto& file : storage.files) {
    files.emplace_back(fcntl(file.descriptor_fd, F_DUPFD_CLOEXEC, 0));
    struct stat stat{};
    if (files.back().get() < 0 || fstat(files.back().get(), &stat) ||
        !S_ISREG(stat.st_mode) || stat.st_size < 0 || static_cast<uint64_t>(stat.st_size) != file.size) {
      *error = "broker storage descriptor is invalid or has wrong size";
      return false;
    }
  }
  return true;
}

bool PosixTransfer(bool write, void* buffer, int fd, size_t offset, size_t size,
                   TransferCancellation* cancellation, std::string* error)
{
  for (size_t completed = 0; completed < size;) {
    if (cancellation && cancellation->IsCancelled()) {
      *error = "allocation transfer canceled";
      return false;
    }
    auto* bytes = static_cast<char*>(buffer);
    ssize_t count;
    do {
      count = write ? pwrite(fd, bytes + completed, size - completed, offset + completed)
                    : pread(fd, bytes + completed, size - completed, offset + completed);
    } while (count < 0 && errno == EINTR);
    if (count <= 0) {
      *error = count ? std::strerror(errno) : "storage transfer made no progress";
      return false;
    }
    completed += count;
  }
  return true;
}

// The same bounded ring used by the PageBroker POSIX CustomStorage adapter:
// saves prime D2H slots then overlap writes with the next copies; loads reuse
// each slot only after its previous H2D completes. No native checkpoint API.
bool TransferPipeline(const std::vector<TransferChunk>& chunks, const std::vector<FileDescriptor>& files,
                      std::vector<std::unique_ptr<TransferSlot>>& slots, CUdeviceptr device, CUstream stream,
                      TransferOperation operation, cuda_checkpoint_storage::ContentDigest& digest,
                      TransferMetrics* metrics, TransferCancellation* cancellation, std::string* error)
{
  const bool save = operation == TransferOperation::kCheckpoint;
  size_t next = 0;
  if (save) {
    for (; next < std::min(chunks.size(), slots.size()); ++next)
      if (!slots[chunks[next].slot_index]->Copy(operation, chunks[next], device, stream, error))
        return false;
  }
  for (const auto& chunk : chunks) {
    auto& slot = *slots[chunk.slot_index];
    if ((cancellation && cancellation->IsCancelled()) || !slot.Wait(metrics, error))
      return false;
    if (save && !digest.Update(slot.data(), chunk.size, error))
      return false;
    const auto start = Clock::now();
    if (!PosixTransfer(save, slot.data(), files[chunk.file_index].get(), chunk.file_offset,
                       chunk.size, cancellation, error))
      return false;
    const double elapsed = ElapsedSeconds(start);
    metrics->storage_io_seconds += elapsed;
    metrics->files[chunk.file_index].storage_io_seconds += elapsed;
    metrics->files[chunk.file_index].bytes += chunk.size;
    if (!save) {
      if (!digest.Update(slot.data(), chunk.size, error) || !slot.Copy(operation, chunk, device, stream, error))
        return false;
    } else if (next < chunks.size()) {
      if (chunks[next].slot_index != chunk.slot_index) {
        *error = "invalid transfer ring layout";
        return false;
      }
      if (!slot.Copy(operation, chunks[next++], device, stream, error))
        return false;
    }
  }
  for (auto& slot : slots)
    if (!slot->Wait(metrics, error))
      return false;
  return true;
}
}  // namespace

bool TransferBackendAvailable() { return true; }

bool TransferExtent(CUdeviceptr device, size_t size, CUstream stream, CUcontext context,
                    const StorageLayout& storage, TransferOperation operation, const TransferOptions& options,
                    TransferCancellation* cancellation, TransferMetrics* metrics, std::string* error)
{
  if (!metrics || !error)
    return false;
  *metrics = {};
  error->clear();
  const auto total_start = Clock::now();
  std::vector<TransferChunk> chunks;
  if (!device || !context || size > std::numeric_limits<CUdeviceptr>::max() - device ||
      !BuildTransferChunks(size, storage, options, &chunks, error) || cuCtxSetCurrent(context) != CUDA_SUCCESS) {
    if (cancellation) cancellation->Cancel();
    return false;
  }
  metrics->files.resize(storage.files.size());
  const auto setup_start = Clock::now();
  std::vector<FileDescriptor> files;
  if (!OpenStorageFiles(storage, files, error))
    return false;
  std::vector<std::unique_ptr<TransferSlot>> slots;
  for (size_t i = 0; i < options.buffer_count; ++i) {
    auto slot = std::make_unique<TransferSlot>();
    const auto status = slot->Allocate(options.chunk_bytes);
    if (status != CUDA_SUCCESS) {
      *error = "allocate registered transfer slot: " + CudaError(status);
      return false;
    }
    slots.push_back(std::move(slot));
  }
  metrics->setup_seconds = ElapsedSeconds(setup_start);
  cuda_checkpoint_storage::ContentDigest digest;
  bool success;
  {
    StreamDrainGuard drain(stream, slots);
    const auto start = Clock::now();
    success = TransferPipeline(chunks, files, slots, device, stream, operation, digest, metrics, cancellation, error);
    metrics->pipeline_seconds = ElapsedSeconds(start);
    if (success) drain.Disarm();
  }
  if (success)
    success = digest.Finalize(&metrics->sha256, error);
  if (success && operation == TransferOperation::kCheckpoint) {
    for (size_t i = 0; i < files.size(); ++i) {
      const auto start = Clock::now();
      const int result = fsync(files[i].get());
      const double elapsed = ElapsedSeconds(start);
      metrics->fsync_seconds += elapsed;
      metrics->files[i].fsync_seconds += elapsed;
      if (result) {
        *error = "sync allocation content failed";
        success = false;
        break;
      }
    }
  }
  const auto cleanup_start = Clock::now();
  for (auto& slot : slots)
    if (!slot->Close(error)) success = false;
  metrics->cleanup_seconds = ElapsedSeconds(cleanup_start);
  metrics->total_seconds = ElapsedSeconds(total_start);
  if (success && cancellation && cancellation->IsCancelled()) {
    *error = "allocation transfer canceled before completion";
    success = false;
  }
  if (success) metrics->bytes = size;
  else if (cancellation) cancellation->Cancel();
  return success;
}
}  // namespace cuda_checkpoint_transfer

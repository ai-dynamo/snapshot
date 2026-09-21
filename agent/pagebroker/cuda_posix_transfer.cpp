// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "cuda_posix_transfer.hpp"
#ifdef PAGEBROKER_NIXL
#include "nixl_transfer.hpp"
#endif

#include <fcntl.h>
#include <unistd.h>
#include <algorithm>
#include <chrono>
#include <cstring>
#include <vector>

namespace snapshot::pagebroker::cuda {
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
    if (mapped_) cuMemUnmap(address_, size_);
    if (allocation_) cuMemRelease(allocation_);
    if (address_) cuMemAddressFree(address_, size_);
  }

  CUresult Allocate(size_t size)
  {
    // Place the persistent staging ring near the GPU that consumes it.
    CUdevice device;
    auto status = cuCtxGetDevice(&device);
    if (status != CUDA_SUCCESS) return status;
    int node = -1;
    status = cuDeviceGetAttribute(&node, CU_DEVICE_ATTRIBUTE_HOST_NUMA_ID, device);
    if (status != CUDA_SUCCESS) return status;
    CUmemAllocationProp properties{};
    properties.type = CU_MEM_ALLOCATION_TYPE_PINNED;
    properties.location.type = CU_MEM_LOCATION_TYPE_HOST_NUMA;
    // CUDA reports -1 without NUMA; host allocations then require node 0.
    properties.location.id = node == -1 ? 0 : node;
    size_t granularity;
    status = cuMemGetAllocationGranularity(&granularity, &properties, CU_MEM_ALLOC_GRANULARITY_MINIMUM);
    if (status != CUDA_SUCCESS) return status;
    size_ = ((size + granularity - 1) / granularity) * granularity;
    status = cuMemCreate(&allocation_, size_, &properties, 0);
    if (status != CUDA_SUCCESS) return status;
    status = cuMemAddressReserve(&address_, size_, granularity, 0, 0);
    if (status != CUDA_SUCCESS) return status;
    status = cuMemMap(address_, size_, 0, allocation_, 0);
    if (status != CUDA_SUCCESS) return status;
    mapped_ = true;
    CUmemAccessDesc access[2]{};
    access[0].location = properties.location;
    access[1].location.type = CU_MEM_LOCATION_TYPE_DEVICE;
    access[1].location.id = device;
    for (auto& descriptor : access) descriptor.flags = CU_MEM_ACCESS_FLAGS_PROT_READWRITE;
    status = cuMemSetAccess(address_, size_, access, 2);
    if (status != CUDA_SUCCESS) return status;
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

  bool Copy(TransferOperation operation, CUdeviceptr device, size_t size,
            CUstream stream, std::string* error)
  {
    // Set before enqueue: failure does not establish that no DMA was posted.
    cuda_may_access_ = true;
    auto status = operation == TransferOperation::kCheckpoint
        ? cuMemcpyDtoHAsync(data(), device, size, stream)
        : cuMemcpyHtoDAsync(device, data(), size, stream);
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
  void* data() const { return reinterpret_cast<void*>(address_); }

 private:
  CUmemGenericAllocationHandle allocation_ = 0;
  CUdeviceptr address_ = 0;
  size_t size_ = 0;
  bool mapped_ = false;
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
    if (!armed_) return;
    // A persistent ring cannot be leased again after an uncertain DMA result.
    // Process exit is the fault boundary when the driver cannot drain it.
    if (cuStreamSynchronize(stream_) != CUDA_SUCCESS) _exit(1);
    for (auto& slot : slots_) slot->Complete();
  }
  void Disarm() { armed_ = false; }
 private:
  CUstream stream_;
  std::vector<std::unique_ptr<TransferSlot>>& slots_;
  bool armed_ = true;
};

#ifndef PAGEBROKER_NIXL
bool PosixTransfer(bool write, void* buffer, int fd, size_t offset, size_t size,
                   std::string* error)
{
  for (size_t completed = 0; completed < size;) {
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

#endif
}  // namespace

struct TransferBuffers::Impl {
  TransferOptions options;
  CUcontext context = nullptr;
  std::vector<std::unique_ptr<TransferSlot>> slots;
#ifdef PAGEBROKER_NIXL
  // Destroy NIXL registrations before their pinned buffers.
  std::unique_ptr<NixlTransfer> storage;
#endif
};

TransferBuffers::TransferBuffers(TransferOptions options) : impl_(std::make_unique<Impl>())
{
  impl_->options = options;
}
TransferBuffers::~TransferBuffers() = default;

bool TransferBuffers::Initialize(CUcontext context, std::string* error)
{
  const auto& options = impl_->options;
  if (!options.buffer_count || !options.chunk_bytes || options.chunk_bytes % 4096) {
    *error = "transfer ring requires buffers with page-aligned capacity";
    return false;
  }
  auto status = cuCtxSetCurrent(context);
  if (status != CUDA_SUCCESS) {
    *error = "set transfer context: " + CudaError(status);
    return false;
  }
  impl_->context = context;
  for (size_t i = 0; i < options.buffer_count; ++i) {
    auto slot = std::make_unique<TransferSlot>();
    status = slot->Allocate(options.chunk_bytes);
    if (status != CUDA_SUCCESS) {
      *error = "allocate CUDA host-NUMA transfer slot: " + CudaError(status);
      return false;
    }
    impl_->slots.push_back(std::move(slot));
  }
#ifdef PAGEBROKER_NIXL
  std::vector<void*> addresses;
  for (const auto& slot : impl_->slots) addresses.push_back(slot->data());
  impl_->storage = std::make_unique<NixlTransfer>(addresses, options.chunk_bytes);
#endif
  return true;
}

bool TransferBuffers::Transfer(int fd, CUdeviceptr device, size_t size, CUstream stream,
                              TransferOperation operation, TransferMetrics* metrics, std::string* error)
{
  *metrics = {};
  error->clear();
  const auto total_start = Clock::now();
  auto status = cuCtxSetCurrent(impl_->context);
  if (status != CUDA_SUCCESS) {
    *error = "set transfer context: " + CudaError(status);
    return false;
  }
  const auto& options = impl_->options;
  if (options.direct_io) {
    if (size % 4096) {
      *error = "direct I/O requires a page-aligned extent";
      return false;
    }
    const int flags = fcntl(fd, F_GETFL);
    if (flags < 0 || fcntl(fd, F_SETFL, flags | O_DIRECT) < 0) {
      *error = "enable direct I/O: " + std::string(std::strerror(errno));
      return false;
    }
  }
  auto& slots = impl_->slots;
  const size_t width = slots.size();
  const size_t count = size / options.chunk_bytes + (size % options.chunk_bytes != 0);
  auto offset = [&](size_t i) { return i * options.chunk_bytes; };
  auto length = [&](size_t i) { return std::min(options.chunk_bytes, size - offset(i)); };
  auto copy = [&](size_t i) {
    return slots[i % width]->Copy(operation, device + offset(i), length(i), stream, error);
  };
  const bool save = operation == TransferOperation::kCheckpoint;
  bool success = true;
  {
    StreamDrainGuard drain(stream, slots);
#ifdef PAGEBROKER_NIXL
    auto& io = *impl_->storage;
    io.Open(fd, size);
#endif
    metrics->setup_seconds = ElapsedSeconds(total_start);
    const auto start = Clock::now();
    try {
      // Prime the complete ring. Reads overlap H2D; writes overlap D2H.
      for (size_t i = 0; i < std::min(width, count); ++i) {
        if (save) {
          if (!copy(i)) { success = false; break; }
        }
#ifdef PAGEBROKER_NIXL
        else io.Submit(i, false, offset(i), length(i));
#endif
      }
      for (size_t i = 0; success && i < count; ++i) {
        const size_t index = i % width;
        auto& slot = *slots[index];
#ifdef PAGEBROKER_NIXL
        if (!io.Wait(index, &metrics->storage_io_seconds, error)) {
          success = false;
          break;
        }
        if (save && i >= width && !copy(i)) { success = false; break; }
#endif
        if (!slot.Wait(metrics, error)) { success = false; break; }
#ifdef PAGEBROKER_NIXL
        if (save) io.Submit(index, true, offset(i), length(i));
        else {
          if (!copy(i) || !slot.Wait(metrics, error)) { success = false; break; }
          if (i + width < count)
            io.Submit(index, false, offset(i + width), length(i + width));
        }
#else
        const auto io_start = Clock::now();
        success = PosixTransfer(save, slot.data(), fd, offset(i), length(i), error);
        metrics->storage_io_seconds += ElapsedSeconds(io_start);
        if (!success) break;
        if (!save) success = copy(i);
        else if (i + width < count) success = copy(i + width);
#endif
      }
#ifdef PAGEBROKER_NIXL
      for (size_t i = 0; i < width; ++i)
        if (!io.Wait(i, &metrics->storage_io_seconds, error)) success = false;
      io.Close();
#endif
      for (auto& slot : slots)
        if (!slot->Wait(metrics, error)) success = false;
    } catch (...) {
#ifdef PAGEBROKER_NIXL
      // Drain storage before the caller can close its file. The guard above
      // also drains DMA before this ring can be reused after an exception.
      io.Close();
#endif
      throw;
    }
    metrics->pipeline_seconds = ElapsedSeconds(start);
    if (success) drain.Disarm();
  }
  if (success && save) {
    const auto start = Clock::now();
    if (fsync(fd)) {
      *error = "sync device content: " + std::string(std::strerror(errno));
      success = false;
    }
    metrics->fsync_seconds = ElapsedSeconds(start);
  }
  metrics->total_seconds = ElapsedSeconds(total_start);
  if (success) metrics->bytes = size;
  return success;
}
}  // namespace snapshot::pagebroker::cuda

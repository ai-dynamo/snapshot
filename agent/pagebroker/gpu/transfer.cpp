// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "transfer.hpp"
#include "errors.hpp"
#include "../errors.hpp"
#include "../io_engine.hpp"
#include "../fatal_cleanup.hpp"
#include <cerrno>
#include <stdexcept>
#include <system_error>
#include <cstdio>
#include <string>
#include <nvtx3/nvtx3.hpp>

#include <unistd.h>
#include <algorithm>
#include <iterator>
#include <limits>
#include <utility>
#include <vector>

namespace snapshot::pagebroker::cuda {
namespace {
// GPU O_DIRECT transfers use 4 KiB-aligned chunk sizes and file offsets.
// This I/O alignment is independent of the host page size.
constexpr size_t kDirectIOAlignment = 4096;

struct TransferChunks {
  size_t bytes;
  size_t capacity;

  size_t Count() const { return bytes / capacity + (bytes % capacity != 0); }
  size_t Offset(size_t index) const { return index * capacity; }
  size_t Length(size_t index) const { return std::min(capacity, bytes - Offset(index)); }
};

CUresult HostAllocationProperties(CUdevice* device, CUmemAllocationProp* properties,
                                  size_t* granularity)
{
  // Place the persistent staging ring near the GPU that consumes it.
  auto status = cuCtxGetDevice(device);
  if (status != CUDA_SUCCESS) {
    return status;
  }
  int node = -1;
  status = cuDeviceGetAttribute(&node, CU_DEVICE_ATTRIBUTE_HOST_NUMA_ID, *device);
  if (status != CUDA_SUCCESS) {
    return status;
  }
  *properties = {};
  properties->type = CU_MEM_ALLOCATION_TYPE_PINNED;
  properties->location.type = CU_MEM_LOCATION_TYPE_HOST_NUMA;
  // Without NUMA, CUDA reports -1 and host allocations require node 0.
  properties->location.id = node == -1 ? 0 : node;
  return cuMemGetAllocationGranularity(granularity, properties, CU_MEM_ALLOC_GRANULARITY_MINIMUM);
}

void CheckCleanup(CUresult result, const char* operation)
{
  if (result != CUDA_SUCCESS) {
    ReportFatalCleanup(gpu::CudaErrorMessage(result, operation).c_str());
  }
}

// Stop cleanup on the first failure, retaining the remaining owners until exit.
struct CudaAllocation {
  CUmemGenericAllocationHandle handle = 0;
  CudaAllocation() = default;
  CudaAllocation(const CudaAllocation&) = delete;
  CudaAllocation& operator=(const CudaAllocation&) = delete;
  ~CudaAllocation()
  {
    if (handle) {
      CheckCleanup(cuMemRelease(handle), "release CUDA allocation");
    }
  }
};

struct CudaMapping {
  CUdeviceptr address = 0;
  size_t size = 0;
  bool mapped = false;
  CudaMapping() = default;
  CudaMapping(const CudaMapping&) = delete;
  CudaMapping& operator=(const CudaMapping&) = delete;
  ~CudaMapping()
  {
    if (mapped) {
      CheckCleanup(cuMemUnmap(address, size), "unmap CUDA transfer buffer");
    }
    if (address) {
      CheckCleanup(cuMemAddressFree(address, size), "free CUDA transfer address");
    }
  }
};

struct CudaEvent {
  CUevent handle = nullptr;
  CudaEvent() = default;
  CudaEvent(const CudaEvent&) = delete;
  CudaEvent& operator=(const CudaEvent&) = delete;
  ~CudaEvent()
  {
    if (handle) {
      CheckCleanup(cuEventDestroy(handle), "destroy CUDA transfer event");
    }
  }
};

class TransferSlot {
 public:
  CUresult Allocate(size_t size)
  {
    CUdevice device;
    CUmemAllocationProp properties{};
    size_t granularity;
    auto status = HostAllocationProperties(&device, &properties, &granularity);
    if (status != CUDA_SUCCESS) {
      return status;
    }
    if (!granularity || size > std::numeric_limits<size_t>::max() - (granularity - 1)) {
      return CUDA_ERROR_INVALID_VALUE;
    }
    mapping_.size = ((size + granularity - 1) / granularity) * granularity;
    status = cuMemCreate(&allocation_.handle, mapping_.size, &properties, 0);
    if (status != CUDA_SUCCESS) {
      return status;
    }
    status = cuMemAddressReserve(&mapping_.address, mapping_.size, granularity, 0, 0);
    if (status != CUDA_SUCCESS) {
      return status;
    }
    status = cuMemMap(mapping_.address, mapping_.size, 0, allocation_.handle, 0);
    if (status != CUDA_SUCCESS) {
      return status;
    }
    mapping_.mapped = true;
    CUmemAccessDesc access[2]{};
    access[0].location = properties.location;
    access[1].location.type = CU_MEM_LOCATION_TYPE_DEVICE;
    access[1].location.id = device;
    for (auto& descriptor : access) {
      descriptor.flags = CU_MEM_ACCESS_FLAGS_PROT_READWRITE;
    }
    status = cuMemSetAccess(mapping_.address, mapping_.size, access, std::size(access));
    if (status != CUDA_SUCCESS) {
      return status;
    }
    return cuEventCreate(&event_.handle, CU_EVENT_DISABLE_TIMING);
  }

  void Wait()
  {
    if (!pending_) {
      return;
    }
    // This wait cannot observe cancellation. The caller checks the token after
    // completion, while this slot keeps the copy's buffer alive. An ordinary
    // Abort or shutdown must wait. The fatal-cleanup watchdog can terminate the
    // process without freeing the buffer if another operation reports a fatal error.
    const nvtx3::scoped_range range{"PageBroker CUDA wait"};
    const auto status = cuEventSynchronize(event_.handle);
    if (status != CUDA_SUCCESS) {
      throw std::runtime_error(gpu::CudaErrorMessage(status, "CUDA event synchronization"));
    }
    Complete();
  }

  void CopyFromDevice(CUdeviceptr device, size_t size, CUstream stream)
  {
    RecordCopy(cuMemcpyDtoHAsync(data(), device, size, stream), stream);
  }

  void CopyToDevice(CUdeviceptr device, size_t size, CUstream stream)
  {
    RecordCopy(cuMemcpyHtoDAsync(device, data(), size, stream), stream);
  }

  void Complete() { pending_ = false; }
  void* data() const { return reinterpret_cast<void*>(mapping_.address); }

 private:
  void RecordCopy(CUresult status, CUstream stream)
  {
    if (status == CUDA_SUCCESS) {
      status = cuEventRecord(event_.handle, stream);
    }
    if (status != CUDA_SUCCESS) {
      throw std::runtime_error(gpu::CudaErrorMessage(status, "CUDA asynchronous copy"));
    }
    pending_ = true;
  }

  CudaAllocation allocation_;
  CudaMapping mapping_;
  CudaEvent event_;
  bool pending_ = false;
};

}  // namespace

size_t TransferMemoryBytes(const TransferOptions& options, size_t device_count, size_t memory_limit_bytes)
{
  Validate(options.buffer_count && options.chunk_bytes && options.chunk_bytes % kDirectIOAlignment == 0,
           "transfer ring requires buffers with 4 KiB-aligned capacity");
  Validate(options.buffer_count <= std::numeric_limits<size_t>::max() / options.chunk_bytes,
           "transfer ring memory size overflows");
  const size_t per_device = options.buffer_count * options.chunk_bytes;
  Validate(device_count <= std::numeric_limits<size_t>::max() / per_device,
           "total transfer ring memory size overflows");
  const size_t total = per_device * device_count;
  Validate(!memory_limit_bytes || total <= memory_limit_bytes,
           "total transfer ring memory exceeds configured limit");
  return total;
}

struct TransferBuffers::Impl {
  TransferOptions options;
  CUcontext context = nullptr;
  std::vector<std::unique_ptr<TransferSlot>> slots;
  // Destroy storage registrations before their pinned buffers.
  std::unique_ptr<TransferEngine> storage;

  void Checkpoint(CUdeviceptr device, size_t size, CUstream stream, const Cancellation& cancellation);
  void Restore(CUdeviceptr device, size_t size, CUstream stream, const Cancellation& cancellation);
  void Transfer(int fd, CUdeviceptr device, size_t size, CUstream stream,
                io::Operation operation, const Cancellation& cancellation);
};

TransferBuffers::TransferBuffers(TransferOptions options) : impl_(std::make_unique<Impl>())
{
  impl_->options = options;
}
TransferBuffers::~TransferBuffers() = default;

size_t TransferBuffers::AllocationBytes(CUcontext context, const TransferOptions& options)
{
  TransferMemoryBytes(options, 1, 0);
  auto status = cuCtxSetCurrent(context);
  CUdevice device;
  CUmemAllocationProp properties{};
  size_t granularity = 0;
  if (status == CUDA_SUCCESS) {
    status = HostAllocationProperties(&device, &properties, &granularity);
  }
  if (status != CUDA_SUCCESS) {
    throw std::runtime_error(gpu::CudaErrorMessage(status, "query transfer allocation granularity"));
  }
  if (!granularity || options.chunk_bytes > std::numeric_limits<size_t>::max() - (granularity - 1)) {
    throw std::invalid_argument("rounded transfer allocation size overflows");
  }
  const size_t rounded = ((options.chunk_bytes + granularity - 1) / granularity) * granularity;
  if (options.buffer_count > std::numeric_limits<size_t>::max() / rounded) {
    throw std::invalid_argument("rounded transfer ring memory size overflows");
  }
  return options.buffer_count * rounded;
}

void TransferBuffers::Initialize(CUcontext context)
{
  const nvtx3::scoped_range range{"PageBroker transfer buffers"};
  const auto& options = impl_->options;
  TransferMemoryBytes(options, 1, 0);
  if (!impl_->slots.empty()) {
    throw std::logic_error("transfer ring is already initialized");
  }
  const auto status = cuCtxSetCurrent(context);
  if (status != CUDA_SUCCESS) {
    throw std::runtime_error(gpu::CudaErrorMessage(status, "set transfer context"));
  }
  std::vector<std::unique_ptr<TransferSlot>> slots;
  for (size_t i = 0; i < options.buffer_count; ++i) {
    auto slot = std::make_unique<TransferSlot>();
    const auto allocated = slot->Allocate(options.chunk_bytes);
    if (allocated != CUDA_SUCCESS) {
      throw std::runtime_error(gpu::CudaErrorMessage(allocated, "allocate CUDA host NUMA buffer"));
    }
    slots.push_back(std::move(slot));
  }
  std::vector<void*> addresses;
  for (const auto& slot : slots) {
    addresses.push_back(slot->data());
  }
  auto storage = std::make_unique<NixlTransferEngine>(addresses, options.chunk_bytes);
  impl_->storage = std::move(storage);
  impl_->slots = std::move(slots);
  impl_->context = context;
}

void TransferBuffers::Impl::Checkpoint(CUdeviceptr device, size_t size, CUstream stream,
                                       const Cancellation& cancellation)
{
  const size_t width = slots.size();
  const TransferChunks chunks{size, options.chunk_bytes};
  const size_t count = chunks.Count();

  // Fill the ring with device copies, then overlap copies with file writes.
  for (size_t i = 0; i < std::min(width, count); ++i) {
    cancellation.ThrowIfCancelled();
    slots[i]->CopyFromDevice(device + chunks.Offset(i), chunks.Length(i), stream);
  }
  for (size_t i = 0; i < count; ++i) {
    cancellation.ThrowIfCancelled();
    const size_t index = i % width;
    auto& slot = *slots[index];
    storage->Wait(index);
    if (i >= width) {
      cancellation.ThrowIfCancelled();
      slot.CopyFromDevice(device + chunks.Offset(i), chunks.Length(i), stream);
    }
    slot.Wait();
    cancellation.ThrowIfCancelled();
    storage->Submit(index, io::Operation::Write, chunks.Offset(i), chunks.Length(i));
  }
}

void TransferBuffers::Impl::Restore(CUdeviceptr device, size_t size, CUstream stream,
                                    const Cancellation& cancellation)
{
  const size_t width = slots.size();
  const TransferChunks chunks{size, options.chunk_bytes};
  const size_t count = chunks.Count();

  // Read ahead into the ring, then copy each completed chunk to the device.
  for (size_t i = 0; i < std::min(width, count); ++i) {
    cancellation.ThrowIfCancelled();
    storage->Submit(i, io::Operation::Read, chunks.Offset(i), chunks.Length(i));
  }
  for (size_t i = 0; i < count; ++i) {
    cancellation.ThrowIfCancelled();
    const size_t index = i % width;
    auto& slot = *slots[index];
    storage->Wait(index);
    slot.Wait();
    cancellation.ThrowIfCancelled();
    slot.CopyToDevice(device + chunks.Offset(i), chunks.Length(i), stream);
    // The next storage read reuses this slot. Finish its CUDA copy first.
    slot.Wait();
    cancellation.ThrowIfCancelled();
    if (i + width < count) {
      storage->Submit(index, io::Operation::Read, chunks.Offset(i + width), chunks.Length(i + width));
    }
  }
}

void TransferBuffers::Impl::Transfer(int fd, CUdeviceptr device, size_t size, CUstream stream,
                                     io::Operation operation, const Cancellation& cancellation)
{
  cancellation.ThrowIfCancelled();
  if (slots.empty()) {
    throw std::logic_error("transfer ring is not initialized");
  }
  const auto status = cuCtxSetCurrent(context);
  if (status != CUDA_SUCCESS) {
    throw std::runtime_error(gpu::CudaErrorMessage(status, "set transfer context"));
  }
  if (options.direct_io && size % kDirectIOAlignment) {
    throw std::invalid_argument("direct I/O requires a 4 KiB-aligned extent");
  }
  if (size) {
    {
      const nvtx3::scoped_range setup{"PageBroker storage setup"};
      storage->Open(fd, size);
    }
    try {
      if (operation == io::Operation::Write) {
        Checkpoint(device, size, stream, cancellation);
      } else {
        Restore(device, size, stream, cancellation);
      }
      for (size_t i = 0; i < slots.size(); ++i) {
        storage->Wait(i);
      }
      storage->Close();
      for (auto& slot : slots) {
        slot->Wait();
      }
      cancellation.ThrowIfCancelled();
    } catch (...) {
      try {
        storage->Close();
      } catch (const std::exception& error) {
        std::fprintf(stderr, "drain storage requests: %s\n", error.what());
      }
      // A failed copy or event call can still leave DMA in flight.
      const auto drained = cuStreamSynchronize(stream);
      if (drained != CUDA_SUCCESS) {
        ReportFatalCleanup(gpu::CudaErrorMessage(drained, "drain CUDA transfer stream").c_str());
      }
      for (auto& slot : slots) {
        slot->Complete();
      }
      throw;
    }
  }
}

void TransferBuffers::Checkpoint(int fd, CUdeviceptr device, size_t size, CUstream stream,
                                 const Cancellation& cancellation)
{
  const nvtx3::scoped_range range{"PageBroker extent transfer"};
  impl_->Transfer(fd, device, size, stream, io::Operation::Write, cancellation);
  const nvtx3::scoped_range sync{"PageBroker file sync"};
  if (fsync(fd)) {
    throw std::system_error(errno, std::generic_category(), "sync GPU extent");
  }
  cancellation.ThrowIfCancelled();
}

void TransferBuffers::Restore(int fd, CUdeviceptr device, size_t size, CUstream stream,
                              const Cancellation& cancellation)
{
  const nvtx3::scoped_range range{"PageBroker extent transfer"};
  impl_->Transfer(fd, device, size, stream, io::Operation::Read, cancellation);
}
}  // namespace snapshot::pagebroker::cuda

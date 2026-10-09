// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "transfer.hpp"
#include "errors.hpp"
#include "../errors.hpp"
#include "../io_engine.hpp"
#include "../fatal_cleanup.hpp"
#include "../file_descriptor.hpp"
#include <cerrno>
#include <stdexcept>
#include <system_error>
#include <string>
#include <nvtx3/nvtx3.hpp>

#include <unistd.h>
#include <fcntl.h>
#include <algorithm>
#include <iterator>
#include <limits>
#include <utility>
#include <vector>
#include <condition_variable>
#include <deque>
#include <map>
#include <mutex>
#include <optional>
#include <thread>
#include <pthread.h>

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

struct CudaStream {
  CUstream handle = nullptr;
  CudaStream() = default;
  CudaStream(const CudaStream&) = delete;
  CudaStream& operator=(const CudaStream&) = delete;
  ~CudaStream()
  {
    if (handle) {
      CheckCleanup(cuStreamDestroy(handle), "destroy pooled transfer stream");
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

  void AllowDevice(CUdevice device)
  {
    CUmemAccessDesc access{};
    access.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
    access.location.id = device;
    access.flags = CU_MEM_ACCESS_FLAGS_PROT_READWRITE;
    gpu::CheckCuda(cuMemSetAccess(mapping_.address, mapping_.size, &access, 1),
                   "allow pooled staging access");
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

TransferFile::TransferFile(int fd, size_t lanes) : file_(FileDescriptor::Duplicate(fd))
{
  const nvtx3::scoped_range range{"PageBroker pooled file open"};
  const int flags = fcntl(fd, F_GETFL);
  if (flags < 0) {
    throw std::system_error(errno, std::generic_category(), "get pooled file flags");
  }
  const auto path = "/proc/self/fd/" + std::to_string(fd);
  for (size_t index = 0; index < lanes; ++index) {
    FileDescriptor lane(open(path.c_str(), O_CLOEXEC | (flags & (O_ACCMODE | O_DIRECT))));
    if (lane.get() < 0) {
      throw std::system_error(errno, std::generic_category(), "prepare pooled file");
    }
    lanes_.push_back(std::move(lane));
  }
}

struct TransferPool::Impl {
  struct Job {
    FileDescriptor file;
    CUcontext context;
    CUdevice ordinal;
    CUdeviceptr device;
    size_t size;
    io::Operation operation;
    const Cancellation& cancellation;
    size_t next = 0;
    size_t active = 0;
    std::exception_ptr failure;
    std::vector<FileDescriptor> lane_files;
    bool Finished() const { return (failure || next == size) && !active; }
  };
  struct Work {
    std::shared_ptr<Job> job;
    size_t slot;
    size_t offset;
    size_t size;
  };
  struct RegisteredFile {
    std::shared_ptr<Job> job;
    FileDescriptor descriptor;
  };
  struct Lane {
    size_t index;
    std::vector<std::unique_ptr<TransferSlot>> slots;
    std::map<CUcontext, std::unique_ptr<CudaEvent>> events;
    std::map<CUcontext, std::unique_ptr<CudaStream>> streams;
    // Keep duplicated descriptors alive until this lane deregisters them.
    std::map<int, RegisteredFile> files;
    std::unique_ptr<NixlTransferEngine> storage;
    std::thread worker;
  };

  TransferOptions options;
  std::vector<std::unique_ptr<Lane>> lanes;
  std::mutex mutex;
  std::condition_variable changed;
  std::deque<std::shared_ptr<Job>> jobs;
  bool stopping = false;
  uint64_t generation = 0;

  Impl(std::span<const CUcontext> contexts, TransferOptions settings, size_t count) : options(settings)
  {
    TransferMemoryBytes(options, contexts.size(), 0);
    const size_t slots = options.buffer_count * contexts.size();
    Validate(!contexts.empty() && count && count <= slots, "invalid transfer pool lane count");
    std::vector<CUdevice> devices;
    std::map<int, std::vector<CUcontext>> numa_contexts;
    for (const auto context : contexts) {
      gpu::CheckCuda(cuCtxSetCurrent(context), "select pooled transfer context");
      CUdevice device;
      gpu::CheckCuda(cuCtxGetDevice(&device), "get pooled transfer device");
      devices.push_back(device);
      int node = -1;
      gpu::CheckCuda(cuDeviceGetAttribute(&node, CU_DEVICE_ATTRIBUTE_HOST_NUMA_ID, device),
                     "get pooled allocation NUMA node");
      numa_contexts[node].push_back(context);
    }
    // Spread staging across host memory controllers even with fewer lanes
    // than GPUs. Consecutive GPU ordinals often share the same NUMA node.
    std::vector<CUcontext> allocation_contexts;
    for (size_t offset = 0; allocation_contexts.size() < contexts.size(); ++offset) {
      for (const auto& [node, local] : numa_contexts) {
        (void)node;
        if (offset < local.size()) allocation_contexts.push_back(local[offset]);
      }
    }
    for (size_t index = 0; index < count; ++index) {
      auto lane = std::make_unique<Lane>();
      lane->index = index;
      for (const auto context : contexts) {
        gpu::CheckCuda(cuCtxSetCurrent(context), "select pooled event context");
        auto event = std::make_unique<CudaEvent>();
        gpu::CheckCuda(cuEventCreate(&event->handle, CU_EVENT_DISABLE_TIMING | CU_EVENT_BLOCKING_SYNC),
                       "create pooled event");
        lane->events.emplace(context, std::move(event));
        auto stream = std::make_unique<CudaStream>();
        gpu::CheckCuda(cuStreamCreate(&stream->handle, CU_STREAM_NON_BLOCKING), "create pooled stream");
        lane->streams.emplace(context, std::move(stream));
      }
      const auto allocation_context = allocation_contexts[index % allocation_contexts.size()];
      gpu::CheckCuda(cuCtxSetCurrent(allocation_context), "select pooled allocation context");
      std::vector<void*> addresses;
      const size_t width = slots / count + (index < slots % count);
      for (size_t slot = 0; slot < width; ++slot) {
        auto buffer = std::make_unique<TransferSlot>();
        gpu::CheckCuda(buffer->Allocate(options.chunk_bytes), "allocate pooled staging");
        for (const auto device : devices) {
          buffer->AllowDevice(device);
        }
        addresses.push_back(buffer->data());
        lane->slots.push_back(std::move(buffer));
      }
      lane->storage = std::make_unique<NixlTransferEngine>(addresses, options.chunk_bytes);
      lanes.push_back(std::move(lane));
    }
    try {
      for (auto& lane : lanes) {
        lane->worker = std::thread([this, p = lane.get()] {
          try {
            pthread_setname_np(pthread_self(), ("pb-lane" + std::to_string(p->index)).c_str());
            Run(*p);
          } catch (...) {
            LogException("pooled transfer worker failed", std::current_exception());
            ReportFatalCleanup("pooled transfer worker could not drain");
          }
        });
      }
    } catch (...) {
      Stop();
      throw;
    }
  }

  ~Impl() { Stop(); }

  void Stop()
  {
    {
      std::lock_guard lock(mutex);
      stopping = true;
      ++generation;
    }
    changed.notify_all();
    for (auto& lane : lanes) {
      if (lane->worker.joinable()) {
        lane->worker.join();
      }
    }
  }

  std::optional<Work> Take(size_t slot)
  {
    std::lock_guard lock(mutex);
    // Assign the oldest unassigned bytes, without waiting for that extent's
    // last copies to drain. Idle lanes can immediately serve the next extent.
    for (const auto& job : jobs) {
      if (job->failure || job->next == job->size) {
        continue;
      }
      try {
        job->cancellation.ThrowIfCancelled();
      } catch (...) {
        job->failure = std::current_exception();
        ++generation;
        changed.notify_all();
        continue;
      }
      const size_t offset = job->next;
      const size_t size = std::min(options.chunk_bytes, job->size - offset);
      job->next += size;
      ++job->active;
      nvtx3::mark(nvtx3::event_attributes{"PageBroker pool dispatch", nvtx3::payload{(uint64_t(job->ordinal) << 32) | uint64_t(offset / options.chunk_bytes)}});
      return Work{job, slot, offset, size};
    }
    return std::nullopt;
  }

  void Complete(const Work& work, std::exception_ptr failure = {})
  {
    std::lock_guard lock(mutex);
    auto& job = *work.job;
    if (failure && !job.failure) {
      job.failure = failure;
    }
    --job.active;
    ++generation;
    nvtx3::mark(nvtx3::event_attributes{"PageBroker pool completion", nvtx3::payload{(uint64_t(job.ordinal) << 32) | uint64_t(work.offset / options.chunk_bytes)}});
    changed.notify_all();
  }

  void Copy(Lane& lane, const Work& work)
  {
    const auto& job = *work.job;
    gpu::CheckCuda(cuCtxSetCurrent(job.context), "select pooled copy context");
    const auto event = lane.events.at(job.context)->handle;
    const auto stream = lane.streams.at(job.context)->handle;
    const auto host = lane.slots[work.slot]->data();
    try {
      const nvtx3::scoped_range range{"PageBroker pooled CUDA copy"};
      {
        const nvtx3::scoped_range enqueue{"PageBroker pooled CUDA enqueue"};
        gpu::CheckCuda(job.operation == io::Operation::Read
            ? cuMemcpyHtoDAsync(job.device + work.offset, host, work.size, stream)
            : cuMemcpyDtoHAsync(host, job.device + work.offset, work.size, stream), "pooled CUDA copy");
        gpu::CheckCuda(cuEventRecord(event, stream), "record pooled CUDA copy");
      }
      const nvtx3::scoped_range wait{"PageBroker pooled CUDA wait"};
      gpu::CheckCuda(cuEventSynchronize(event), "wait pooled CUDA copy");
    } catch (...) {
      CheckCleanup(cuStreamSynchronize(stream), "drain pooled CUDA stream");
      throw;
    }
  }

  void ReleaseFiles(Lane& lane)
  {
    // Finished jobs no longer read their cancellation token, which may have
    // been destroyed after the calling transfer returned.
    for (auto it = lane.files.begin(); it != lane.files.end();) {
      bool finished;
      {
        std::lock_guard lock(mutex);
        finished = it->second.job->Finished();
      }
      if (finished) {
        lane.storage->UnregisterFile(it->second.descriptor.get());
        it = lane.files.erase(it);
      } else {
        ++it;
      }
    }
  }

  void Run(Lane& lane)
  {
    std::deque<size_t> free;
    std::deque<Work> pending;
    for (size_t slot = 0; slot < lane.slots.size(); ++slot) {
      free.push_back(slot);
    }
    for (;;) {
      uint64_t observed;
      {
        std::lock_guard lock(mutex);
        observed = generation;
      }
      ReleaseFiles(lane);
      while (!free.empty()) {
        auto work = Take(free.front());
        if (!work) {
          break;
        }
        free.pop_front();
        try {
          const auto& job = *work->job;
          if (!lane.files.contains(job.file.get())) {
            // Independent open descriptions avoid sharing filesystem state
            // between concurrent submitters. Reopen the held file, not its path.
            const nvtx3::scoped_range registration{"PageBroker pooled file registration"};
            const int flags = fcntl(job.file.get(), F_GETFL);
            if (flags < 0) {
              throw std::system_error(errno, std::generic_category(), "get pooled file flags");
            }
            const auto path = "/proc/self/fd/" + std::to_string(job.file.get());
            FileDescriptor descriptor(job.lane_files.empty()
                ? FileDescriptor(open(path.c_str(), O_CLOEXEC | (flags & (O_ACCMODE | O_DIRECT))))
                : FileDescriptor::Duplicate(job.lane_files.at(lane.index).get()));
            if (descriptor.get() < 0) {
              throw std::system_error(errno, std::generic_category(), "reopen pooled file");
            }
            const int fd = descriptor.get();
            lane.files.emplace(job.file.get(), RegisteredFile{work->job, std::move(descriptor)});
            try {
              lane.storage->RegisterFile(fd, job.size);
            } catch (...) {
              lane.files.erase(job.file.get());
              throw;
            }
          }
          if (job.operation == io::Operation::Write) {
            Copy(lane, *work);
          }
          lane.storage->Submit(work->slot, lane.files.at(job.file.get()).descriptor.get(),
                               job.operation, work->offset, work->size);
          pending.push_back(*work);
        } catch (...) {
          const auto failure = std::current_exception();
          // Submit can throw after creating a request. Drain it before making
          // its slot available or allowing the caller to close the file.
          try { lane.storage->Wait(work->slot); } catch (...) {}
          Complete(*work, failure);
          free.push_back(work->slot);
        }
      }
      if (pending.empty()) {
        std::unique_lock lock(mutex);
        if (stopping) {
          break;
        }
        // Observe changes before checking files and work so a completion or
        // newly queued job cannot be lost between Take and this wait.
        changed.wait(lock, [&] { return stopping || generation != observed; });
        continue;
      }
      auto work = pending.front();
      pending.pop_front();
      std::exception_ptr failure;
      try {
        lane.storage->Wait(work.slot);
        work.job->cancellation.ThrowIfCancelled();
        if (work.job->operation == io::Operation::Read) {
          Copy(lane, work);
        }
      } catch (...) {
        failure = std::current_exception();
      }
      Complete(work, failure);
      free.push_back(work.slot);
    }
    lane.storage->Close();
    lane.files.clear();
  }

  void Transfer(int fd, CUcontext context, CUdeviceptr device, size_t size, CUstream stream,
                io::Operation operation, const Cancellation& cancellation,
                std::span<const FileDescriptor> prepared = {})
  {
    cancellation.ThrowIfCancelled();
    Validate(!options.direct_io || size % kDirectIOAlignment == 0,
             "direct I/O requires a 4 KiB-aligned extent");
    Validate(lanes.front()->events.contains(context), "unknown pooled transfer context");
    if (!size) {
      return;
    }
    // Establish any dependencies on the driver's stream before independent
    // lanes use their own streams. Each lane drains its copies before the job
    // returns, so the driver's later completion call cannot outlive our DMA.
    CUdevice ordinal = 0;
    {
      const nvtx3::scoped_range ready{"PageBroker pooled stream readiness"};
      gpu::CheckCuda(cuCtxSetCurrent(context), "select pooled extent context");
      gpu::CheckCuda(cuCtxGetDevice(&ordinal), "get pooled extent device");
      gpu::CheckCuda(cuStreamSynchronize(stream), "wait caller transfer stream");
    }
    auto job = std::make_shared<Job>(Job{FileDescriptor::Duplicate(fd), context, ordinal, device,
                                        size, operation, cancellation, 0, 0, {}, {}});
    Validate(prepared.empty() || prepared.size() == lanes.size(), "prepared file lane count mismatch");
    for (const auto& file : prepared) {
      job->lane_files.push_back(FileDescriptor::Duplicate(file.get()));
    }
    std::unique_lock lock(mutex);
    jobs.push_back(job);
    ++generation;
    changed.notify_all();
    while (!job->Finished()) {
      changed.wait_for(lock, std::chrono::milliseconds{10});
      if (!job->failure) {
        try {
          cancellation.ThrowIfCancelled();
        } catch (...) {
          job->failure = std::current_exception();
          ++generation;
          changed.notify_all();
        }
      }
    }
    std::erase(jobs, job);
    ++generation;
    changed.notify_all();
    if (job->failure) {
      std::rethrow_exception(job->failure);
    }
    cancellation.ThrowIfCancelled();
  }
};

TransferPool::TransferPool(std::span<const CUcontext> contexts, TransferOptions options, size_t lanes)
    : impl_(std::make_unique<Impl>(contexts, options, lanes)) {}
TransferPool::~TransferPool() = default;

void TransferPool::Restore(const TransferFile& file, CUcontext context, CUdeviceptr device, size_t size,
                           CUstream stream, const Cancellation& cancellation)
{
  const nvtx3::scoped_range range{"PageBroker pooled extent transfer"};
  impl_->Transfer(file.fd(), context, device, size, stream, io::Operation::Read, cancellation, file.lanes_);
}

void TransferPool::Restore(int fd, CUcontext context, CUdeviceptr device, size_t size,
                           CUstream stream, const Cancellation& cancellation)
{
  const nvtx3::scoped_range range{"PageBroker pooled extent transfer"};
  impl_->Transfer(fd, context, device, size, stream, io::Operation::Read, cancellation);
}

void TransferPool::Checkpoint(int fd, CUcontext context, CUdeviceptr device, size_t size,
                              CUstream stream, const Cancellation& cancellation)
{
  const nvtx3::scoped_range range{"PageBroker pooled extent transfer"};
  impl_->Transfer(fd, context, device, size, stream, io::Operation::Write, cancellation);
  const nvtx3::scoped_range sync{"PageBroker file sync"};
  if (fsync(fd)) {
    throw std::system_error(errno, std::generic_category(), "sync pooled GPU extent");
  }
  cancellation.ThrowIfCancelled();
}
}  // namespace snapshot::pagebroker::cuda

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "gpu_transfer.hpp"
#include "gpu/errors.hpp"
#include "errors.hpp"
#include "io_engine.hpp"
#include "fatal_cleanup.hpp"
#include "file_descriptor.hpp"
#include "gpu/storage_manifest.hpp"
#include <atomic>
#include <charconv>
#include <climits>
#include <filesystem>
#include <future>
#include <sys/stat.h>
#include <cerrno>
#include <stdexcept>
#include <system_error>
#include <cstdio>
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

namespace snapshot::pagebroker::posix {
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
  // Pin host memory on the NUMA node of the current context's GPU.
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

class TransferBuffer {
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
    return CUDA_SUCCESS;
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

  void* data() const { return reinterpret_cast<void*>(mapping_.address); }

 private:
  CudaAllocation allocation_;
  CudaMapping mapping_;
};

void RecordCopy(CUresult status, CUevent event, CUstream stream)
{
  if (status == CUDA_SUCCESS) status = cuEventRecord(event, stream);
  gpu::CheckCuda(status, "CUDA asynchronous copy");
}

void WaitForCopy(CUevent event)
{
  const nvtx3::scoped_range range{"PageBroker CUDA wait"};
  gpu::CheckCuda(cuEventSynchronize(event), "CUDA event synchronization");
}

class TransferSlot {
 public:
  CUresult Allocate(size_t size)
  {
    const auto status = buffer_.Allocate(size);
    if (status != CUDA_SUCCESS) return status;
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
    WaitForCopy(event_.handle);
    Complete();
  }

  void CopyFromDevice(CUdeviceptr device, size_t size, CUstream stream)
  {
    RecordCopy(cuMemcpyDtoHAsync(data(), device, size, stream), event_.handle, stream);
    pending_ = true;
  }

  void CopyToDevice(CUdeviceptr device, size_t size, CUstream stream)
  {
    RecordCopy(cuMemcpyHtoDAsync(device, data(), size, stream), event_.handle, stream);
    pending_ = true;
  }

  void Complete() { pending_ = false; }
  void* data() const { return buffer_.data(); }

 private:
  TransferBuffer buffer_;
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

struct TransferPoolDevice::Impl {
  CUcontext context;
  CUdevice device;
  int numa_node = -1;
  TransferOptions options;
  std::vector<std::unique_ptr<TransferBuffer>> slots;
  std::vector<std::unique_ptr<CudaEvent>> events;
  std::vector<std::unique_ptr<CudaStream>> streams;

  Impl(CUcontext context, TransferOptions options, size_t lanes) : context(context), options(options)
  {
    TransferMemoryBytes(options, 1, 0);
    Validate(lanes > 0, "invalid transfer pool lane count");
    gpu::CheckCuda(cuCtxSetCurrent(context), "select pooled transfer context");
    gpu::CheckCuda(cuCtxGetDevice(&device), "get pooled transfer device");
    gpu::CheckCuda(cuDeviceGetAttribute(&numa_node, CU_DEVICE_ATTRIBUTE_HOST_NUMA_ID, device),
                   "get pooled allocation NUMA node");
    for (size_t index = 0; index < lanes; ++index) {
      auto event = std::make_unique<CudaEvent>();
      gpu::CheckCuda(cuEventCreate(&event->handle, CU_EVENT_DISABLE_TIMING | CU_EVENT_BLOCKING_SYNC),
                     "create pooled event");
      events.push_back(std::move(event));
      auto stream = std::make_unique<CudaStream>();
      gpu::CheckCuda(cuStreamCreate(&stream->handle, CU_STREAM_NON_BLOCKING), "create pooled stream");
      streams.push_back(std::move(stream));
    }
    for (size_t index = 0; index < options.buffer_count; ++index) {
      auto slot = std::make_unique<TransferBuffer>();
      gpu::CheckCuda(slot->Allocate(options.chunk_bytes), "allocate pooled staging");
      slots.push_back(std::move(slot));
    }
  }
};

TransferPoolDevice::TransferPoolDevice(CUcontext context, TransferOptions options, size_t lanes)
    : impl_(std::make_unique<Impl>(context, options, lanes)) {}
TransferPoolDevice::~TransferPoolDevice() = default;

void TransferPoolDevice::AllowAccess(std::span<TransferPoolDevice* const> devices)
{
  gpu::CheckCuda(cuCtxSetCurrent(impl_->context), "select pooled access context");
  for (const auto* device : devices) {
    for (const auto& slot : device->impl_->slots) slot->AllowDevice(impl_->device);
  }
}

namespace {
// Owns the POSIX/NIXL lifetime for one worker. Each active file has an
// independent open description, avoiding NFS contention between workers.
// Registration and descriptors live only as long as this lane's requests.
class LaneStorage {
 public:
  LaneStorage(std::span<void* const> addresses, size_t capacity, size_t depth)
      : storage_(addresses, capacity, {}, depth), requests_(addresses.size()) {}

  void Submit(size_t slot, int source, size_t extent_size, io::Operation operation,
              size_t offset, size_t size)
  {
    const Key key{source, extent_size};
    auto found = files_.find(key);
    if (found == files_.end()) {
      const int flags = fcntl(source, F_GETFL);
      if (flags < 0) throw std::system_error(errno, std::generic_category(), "get pooled file flags");
      const auto path = "/proc/self/fd/" + std::to_string(source);
      FileDescriptor descriptor(open(path.c_str(), O_CLOEXEC | (flags & (O_ACCMODE | O_DIRECT))));
      if (descriptor.get() < 0) throw std::system_error(errno, std::generic_category(), "open pooled file");
      found = files_.emplace(key, File{std::move(descriptor), 0}).first;
      try {
        storage_.RegisterFile(found->second.descriptor.get(), extent_size);
      } catch (...) {
        files_.erase(found);
        throw;
      }
    }
    ++found->second.active;
    requests_.at(slot) = key;
    // Wait also cleans up a request when Submit throws after creating it.
    storage_.Submit(slot, found->second.descriptor.get(), operation, offset, size);
  }

  void Wait(size_t slot)
  {
    std::exception_ptr failure;
    try { storage_.Wait(slot); } catch (...) { failure = std::current_exception(); }
    const auto key = std::exchange(requests_.at(slot), std::nullopt);
    if (key) {
      auto found = files_.find(*key);
      if (!--found->second.active) {
        storage_.UnregisterFile(found->second.descriptor.get());
        files_.erase(found);
      }
    }
    if (failure) std::rethrow_exception(failure);
  }

 private:
  using Key = std::pair<int, size_t>;
  struct File { FileDescriptor descriptor; size_t active; };
  // Destroy the NIXL engine before descriptors if exceptional cleanup is needed.
  std::map<Key, File> files_;
  NixlTransferEngine storage_;
  std::vector<std::optional<Key>> requests_;
};
}  // namespace

struct TransferPool::Impl {
  struct Job {
    std::shared_ptr<const TransferFile> file;
    CUcontext context;
    CUdeviceptr device;
    size_t size;
    io::Operation operation;
    const Cancellation& cancellation;
    size_t next = 0;
    size_t active = 0;
    std::exception_ptr failure;
    bool Finished() const { return (failure || next == size) && !active; }
  };
  struct Work {
    std::shared_ptr<Job> job;
    size_t slot;
    size_t offset;
    size_t size;
  };
  struct Lane {
    size_t index;
    size_t depth;
    std::unique_ptr<LaneStorage> storage;
    std::thread worker;
  };
  struct Slot { TransferBuffer* buffer; int numa_node; };

  TransferOptions options;
  std::vector<std::unique_ptr<TransferPoolDevice>> devices;
  std::map<CUcontext, TransferPoolDevice::Impl*> contexts;
  std::vector<Slot> slots;
  std::map<int, std::deque<size_t>> free;
  std::vector<std::unique_ptr<Lane>> lanes;
  std::mutex mutex;
  std::condition_variable changed;
  std::deque<std::shared_ptr<Job>> jobs;
  bool stopping = false;
  uint64_t generation = 0;

  explicit Impl(std::vector<std::unique_ptr<TransferPoolDevice>> prepared) : devices(std::move(prepared))
  {
    Validate(!devices.empty(), "transfer pool requires a device");
    options = devices.front()->impl_->options;
    const size_t requested = devices.front()->impl_->streams.size();
    std::vector<void*> addresses;
    for (const auto& device : devices) {
      auto& state = *device->impl_;
      Validate(state.options.buffer_count == options.buffer_count &&
               state.options.chunk_bytes == options.chunk_bytes &&
               state.options.direct_io == options.direct_io && state.streams.size() == requested,
               "inconsistent transfer pool devices");
      contexts.emplace(state.context, &state);
      for (const auto& slot : state.slots) {
        free[state.numa_node].push_back(slots.size());
        slots.push_back({slot.get(), state.numa_node});
        addresses.push_back(slot->data());
      }
    }
    // A GPU skipped during setup reduces the slot budget. Keep the remaining
    // GPUs available even when fewer slots than requested workers survive.
    const size_t count = std::min(requested, slots.size());
    for (size_t index = 0; index < count; ++index) {
      auto lane = std::make_unique<Lane>();
      lane->index = index;
      lane->depth = (slots.size() + count - 1) / count;
      lane->storage = std::make_unique<LaneStorage>(addresses, options.chunk_bytes, lane->depth);
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

  std::optional<Work> Take()
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
      auto available = free.find(contexts.at(job->context)->numa_node);
      if (available == free.end() || available->second.empty()) {
        available = std::find_if(free.begin(), free.end(), [](const auto& item) { return !item.second.empty(); });
      }
      if (available == free.end()) return std::nullopt;
      const auto slot = available->second.front();
      available->second.pop_front();
      const size_t offset = job->next;
      const size_t size = std::min(options.chunk_bytes, job->size - offset);
      job->next += size;
      ++job->active;
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
    free.at(slots[work.slot].numa_node).push_back(work.slot);
    ++generation;
    changed.notify_all();
  }

  void Copy(Lane& lane, const Work& work)
  {
    const auto& job = *work.job;
    gpu::CheckCuda(cuCtxSetCurrent(job.context), "select pooled copy context");
    const auto& device = *contexts.at(job.context);
    const auto event = device.events[lane.index]->handle;
    const auto stream = device.streams[lane.index]->handle;
    const auto host = slots[work.slot].buffer->data();
    try {
      RecordCopy(job.operation == io::Operation::Read
          ? cuMemcpyHtoDAsync(job.device + work.offset, host, work.size, stream)
          : cuMemcpyDtoHAsync(host, job.device + work.offset, work.size, stream), event, stream);
      WaitForCopy(event);
    } catch (...) {
      CheckCleanup(cuStreamSynchronize(stream), "drain pooled CUDA stream");
      throw;
    }
  }

  void Run(Lane& lane)
  {
    std::deque<Work> pending;
    for (;;) {
      uint64_t observed;
      {
        std::lock_guard lock(mutex);
        observed = generation;
      }
      while (pending.size() < lane.depth) {
        auto work = Take();
        if (!work) {
          break;
        }
        try {
          const auto& job = *work->job;
          if (job.operation == io::Operation::Write) {
            Copy(lane, *work);
          }
          lane.storage->Submit(work->slot, job.file->fd(), job.size, job.operation, work->offset, work->size);
          pending.push_back(*work);
        } catch (...) {
          const auto failure = std::current_exception();
          // Submit can leave a request behind. Drain it before reusing the
          // slot or unregistering the file.
          try { lane.storage->Wait(work->slot); } catch (...) {}
          Complete(*work, failure);
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
        {
          std::lock_guard lock(mutex);
          failure = work.job->failure;
        }
        if (!failure && work.job->operation == io::Operation::Read) {
          Copy(lane, work);
        }
      } catch (...) {
        failure = std::current_exception();
      }
      Complete(work, failure);
    }
  }

  void Transfer(std::shared_ptr<const TransferFile> file, CUcontext context, CUdeviceptr device, size_t size,
                CUstream stream, io::Operation operation, const Cancellation& cancellation)
  {
    cancellation.ThrowIfCancelled();
    Validate(!options.direct_io || size % kDirectIOAlignment == 0,
             "direct I/O requires a 4 KiB-aligned extent");
    Validate(contexts.contains(context), "unknown pooled transfer context");
    if (!size) {
      return;
    }
    // Establish any dependencies on the driver's stream before independent
    // lanes use their own streams. Each lane drains its copies before the job
    // returns, so the driver's later completion call cannot outlive our DMA.
    gpu::CheckCuda(cuCtxSetCurrent(context), "select pooled extent context");
    gpu::CheckCuda(cuStreamSynchronize(stream), "wait caller transfer stream");
    auto job = std::make_shared<Job>(Job{std::move(file), context, device, size, operation, cancellation, 0, 0, {}});
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

TransferPool::TransferPool(std::vector<std::unique_ptr<TransferPoolDevice>> devices)
    : impl_(std::make_unique<Impl>(std::move(devices))) {}
TransferPool::~TransferPool() = default;

void TransferPool::Restore(std::shared_ptr<const TransferFile> file, CUcontext context, CUdeviceptr device,
                           size_t size, CUstream stream, const Cancellation& cancellation)
{
  const nvtx3::scoped_range range{"PageBroker pooled extent transfer"};
  impl_->Transfer(std::move(file), context, device, size, stream, io::Operation::Read, cancellation);
}

void TransferPool::Restore(int fd, CUcontext context, CUdeviceptr device, size_t size,
                           CUstream stream, const Cancellation& cancellation)
{
  Restore(std::make_shared<TransferFile>(fd), context, device, size, stream, cancellation);
}

void TransferPool::Checkpoint(int fd, CUcontext context, CUdeviceptr device, size_t size,
                              CUstream stream, const Cancellation& cancellation)
{
  const nvtx3::scoped_range range{"PageBroker pooled extent transfer"};
  impl_->Transfer(std::make_shared<TransferFile>(fd), context, device, size, stream,
                  io::Operation::Write, cancellation);
  const nvtx3::scoped_range sync{"PageBroker file sync"};
  if (fsync(fd)) {
    throw std::system_error(errno, std::generic_category(), "sync pooled GPU extent");
  }
  cancellation.ThrowIfCancelled();
}
struct GpuRestorePreparation::State {
  using Files = std::map<std::pair<uint32_t, std::string>, std::shared_ptr<TransferFile>>;
  std::atomic<bool> cancelled = false;
  // Declared last so destruction joins before releasing the cancellation token.
  std::shared_future<Files> pending;

  explicit State(int directory_fd)
  {
    pending = std::async(std::launch::async,
        [root = FileDescriptor::Duplicate(directory_fd), this]() mutable {
      auto source = std::move(root);
      const nvtx3::scoped_range range{"PageBroker restore file preparation"};
      try {
        return Scan(source);
      } catch (...) {
        LogException("GPU restore file preparation unavailable", std::current_exception());
        return Files{};
      }
    });
  }
  ~State() { cancelled.store(true); }

  Files Scan(const FileDescriptor& source)
  {
    Files files;
    source.RequireDirectory();
    FileDescriptor gpu_root(openat(source.get(), gpu::kDataDirectory, O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW));
    if (gpu_root.get() < 0 && errno == ENOENT) return files;  // CPU-only checkpoint.
    if (gpu_root.get() < 0) {
      throw std::system_error(errno, std::generic_category(), "open GPU preparation directory");
    }
    const auto path = "/proc/self/fd/" + std::to_string(gpu_root.get());
    for (const auto& entry : std::filesystem::directory_iterator(path)) {
      if (cancelled.load()) return Files{};
      const auto name = entry.path().filename().string();
      uint32_t pid = 0;
      const auto [end, error] = std::from_chars(name.data(), name.data() + name.size(), pid);
      if (error != std::errc{} || end != name.data() + name.size() || !pid ||
          pid > INT_MAX || name != std::to_string(pid)) continue;
      try {
        PrepareParticipant(files, gpu_root.get(), pid, name);
      } catch (...) {
        // The request's captured PID list is not available yet. Its participants
        // are validated again when the GPU request constructs the artifact.
        LogException("skip GPU restore preparation participant", std::current_exception());
      }
    }
    return files;
  }

  void PrepareParticipant(Files& files, int parent, uint32_t pid, const std::string& name)
  {
    auto directory = FileDescriptor::OpenDirectory(parent, name.c_str());
    std::vector<gpu::storage::GpuDataFile> manifest;
    std::string message;
    const auto path = "/proc/self/fd/" + std::to_string(directory.get());
    if (!gpu::storage::ReadManifest(path, &manifest, &message) ||
        !gpu::storage::ValidateExtentFiles(path, manifest, &message)) {
      throw std::runtime_error(message);
    }
    for (const auto& extent : manifest) {
      if (cancelled.load()) return;
      FileDescriptor file(openat(directory.get(), extent.filename.c_str(),
                                 O_RDONLY | O_CLOEXEC | O_NOFOLLOW | O_DIRECT));
      if (file.get() < 0) {
        throw std::system_error(errno, std::generic_category(), "prepare GPU extent");
      }
      files.emplace(std::make_pair(pid, extent.filename), std::make_shared<TransferFile>(file.get()));
    }
  }

  std::shared_ptr<TransferFile> Get(uint32_t pid, const gpu::storage::GpuDataFile& extent, int directory)
  {
    const nvtx3::scoped_range range{"PageBroker restore preparation wait"};
    // Artifact construction holds the transaction mutex while waiting here.
    // Destruction cancels the scan between entries, but transaction cancellation
    // cannot interrupt this wait or an in-progress filesystem call.
    const auto& files = pending.get();
    const auto found = files.find({pid, extent.filename});
    if (found == files.end()) return {};  // Open normally when optional preparation failed.
    const auto& file = found->second;
    struct stat held{}, current{};
    Require(fstat(file->fd(), &held) == 0 &&
             fstatat(directory, extent.filename.c_str(), &current, AT_SYMLINK_NOFOLLOW) == 0 &&
             S_ISREG(held.st_mode) && held.st_dev == current.st_dev && held.st_ino == current.st_ino &&
             held.st_size >= 0 && static_cast<uint64_t>(held.st_size) == extent.size,
             "GPU extent changed after preparation");
    return file;
  }
};

GpuRestorePreparation::GpuRestorePreparation(int directory_fd)
    : state_(std::make_unique<State>(directory_fd)) {}
GpuRestorePreparation::~GpuRestorePreparation() = default;

std::shared_ptr<TransferFile>
GpuRestorePreparation::Get(uint32_t pid, const gpu::storage::GpuDataFile& extent, int directory)
{
  return state_->Get(pid, extent, directory);
}

}  // namespace snapshot::pagebroker::posix

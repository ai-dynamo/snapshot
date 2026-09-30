// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "transfer.hpp"
#include "content_digest.hpp"
#ifdef PAGEBROKER_NIXL
#include <nixl.h>
#include <nixl_descriptors.h>
#include <nixl_params.h>
#include <stdexcept>
#include <thread>
#endif

#include <fcntl.h>
#include <unistd.h>
#include <algorithm>
#include <chrono>
#include <cstring>
#include <limits>
#include <vector>

namespace snapshot::pagebroker::cuda {
namespace {
using Clock = std::chrono::steady_clock;
double ElapsedSeconds(Clock::time_point start)
{
  return std::chrono::duration<double>(Clock::now() - start).count();
}

#ifdef PAGEBROKER_NIXL
// NIXL owns the storage backend contract. This private helper only retains
// registrations and requests for the pipeline's existing host-buffer slots.
// Wait/Close must drain I/O before CUDA can reuse a slot or its file is closed.
void CheckNixl(nixl_status_t status, const char* operation)
{
  if (status != NIXL_SUCCESS)
    throw std::runtime_error(std::string("NIXL ") + operation + ": " + std::to_string(status));
}

class NixlTransfer {
 public:
  NixlTransfer(const std::vector<void*>& buffers, size_t capacity);
  ~NixlTransfer();
  void Open(int descriptor, size_t size);
  void Submit(size_t slot, bool write, size_t offset, size_t size);
  bool Wait(size_t slot, double* seconds, std::string* error);
  void Close();

 private:
  struct Request {
    nixlXferReqH* handle = nullptr;
    nixl_status_t status = NIXL_SUCCESS;
    Clock::time_point started;
  };
  std::string name_;
  std::unique_ptr<nixlAgent> agent_;
  nixl_reg_dlist_t buffers_{DRAM_SEG};
  nixl_reg_dlist_t file_{FILE_SEG};
  std::vector<void*> addresses_;
  std::vector<Request> requests_;
  int descriptor_ = -1;
};

NixlTransfer::NixlTransfer(const std::vector<void*>& buffers, size_t capacity)
{
  static std::atomic<unsigned> sequence{0};
  name_ = "pagebroker-" + std::to_string(getpid()) + "-" + std::to_string(sequence.fetch_add(1));
  nixlAgentConfig config;
  config.useProgThread = true;
  agent_ = std::make_unique<nixlAgent>(name_, config);
  nixl_b_params_t parameters;
  parameters["use_aio"] = "true";
  nixlBackendH* backend = nullptr;
  CheckNixl(agent_->createBackend("POSIX", parameters, backend), "create POSIX backend");
  addresses_ = buffers;
  requests_.resize(buffers.size());
  for (void* address : buffers)
    buffers_.addDesc(nixlBlobDesc(reinterpret_cast<uintptr_t>(address), capacity, 0));
  CheckNixl(agent_->registerMem(buffers_), "register buffers");
}

NixlTransfer::~NixlTransfer()
{
  // POSIX releaseXferReq does not cancel queued AIO callbacks. On failure,
  // reclaim neither request nor buffer until storage completion is known.
  try {
    Close();
    CheckNixl(agent_->deregisterMem(buffers_), "deregister buffers");
  } catch (...) {
    _exit(1);
  }
}

void NixlTransfer::Open(int descriptor, size_t size)
{
  if (descriptor_ != -1)
    throw std::logic_error("NIXL file already open");
  file_.clear();
  file_.addDesc(nixlBlobDesc(0, size, descriptor));
  CheckNixl(agent_->registerMem(file_), "register file");
  descriptor_ = descriptor;
}

void NixlTransfer::Submit(size_t slot, bool write, size_t offset, size_t size)
{
  auto& request = requests_.at(slot);
  if (request.handle || descriptor_ == -1)
    throw std::logic_error("NIXL slot or file is not ready");
  nixl_xfer_dlist_t local(DRAM_SEG);
  nixl_xfer_dlist_t remote(FILE_SEG);
  local.addDesc(nixlBlobDesc(reinterpret_cast<uintptr_t>(addresses_.at(slot)), size, 0));
  remote.addDesc(nixlBlobDesc(offset, size, descriptor_));
  request.started = Clock::now();
  CheckNixl(agent_->createXferReq(write ? NIXL_WRITE : NIXL_READ, local, remote,
                                  name_, request.handle), "create request");
  request.status = agent_->postXferReq(request.handle);
  if (request.status != NIXL_IN_PROG)
    CheckNixl(request.status, "post request");
}

bool NixlTransfer::Wait(size_t slot,
                        double* seconds, std::string* error)
{
  auto& request = requests_.at(slot);
  if (!request.handle)
    return true;
  // A stuck backend is isolated by worker termination, never by releasing
  // registrations that an outstanding request may still access.
  const auto deadline = Clock::now() + std::chrono::seconds(240);
  while (request.status == NIXL_IN_PROG) {
    request.status = agent_->getXferStatus(request.handle);
    if (Clock::now() >= deadline)
      _exit(1);
    if (request.status == NIXL_IN_PROG)
      std::this_thread::sleep_for(std::chrono::microseconds(50));
  }
  *seconds += std::chrono::duration<double>(Clock::now() - request.started).count();
  CheckNixl(agent_->releaseXferReq(request.handle), "release completed request");
  request.handle = nullptr;
  if (request.status != NIXL_SUCCESS) {
    *error = "NIXL transfer failed: " + std::to_string(request.status);
    return false;
  }
  return true;
}

void NixlTransfer::Close()
{
  std::string error;
  double seconds = 0;
  bool success = true;
  for (size_t slot = 0; slot < requests_.size(); ++slot)
    if (!Wait(slot, &seconds, &error)) success = false;
  if (descriptor_ != -1) {
    CheckNixl(agent_->deregisterMem(file_), "deregister file");
    descriptor_ = -1;
  }
  if (!success) throw std::runtime_error(error);
}
#endif

std::string CudaError(CUresult status)
{
  const char* name = nullptr;
  cuGetErrorName(status, &name);
  return name ? name : "unknown CUDA error";
}

CUresult HostAllocationProperties(CUdevice* device, CUmemAllocationProp* properties,
                                  size_t* granularity)
{
  // Place the persistent staging ring near the GPU that consumes it.
  auto status = cuCtxGetDevice(device);
  if (status != CUDA_SUCCESS) return status;
  int node = -1;
  status = cuDeviceGetAttribute(&node, CU_DEVICE_ATTRIBUTE_HOST_NUMA_ID, *device);
  if (status != CUDA_SUCCESS) return status;
  *properties = {};
  properties->type = CU_MEM_ALLOCATION_TYPE_PINNED;
  properties->location.type = CU_MEM_LOCATION_TYPE_HOST_NUMA;
  // CUDA reports -1 without NUMA; host allocations then require node 0.
  properties->location.id = node == -1 ? 0 : node;
  return cuMemGetAllocationGranularity(granularity, properties, CU_MEM_ALLOC_GRANULARITY_MINIMUM);
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
    CUdevice device;
    CUmemAllocationProp properties{};
    size_t granularity;
    auto status = HostAllocationProperties(&device, &properties, &granularity);
    if (status != CUDA_SUCCESS) return status;
    if (!granularity || size > std::numeric_limits<size_t>::max() - (granularity - 1))
      return CUDA_ERROR_INVALID_VALUE;
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

bool ValidateTransferMemory(const TransferOptions& options, size_t device_count,
                            size_t memory_limit_bytes, size_t* total_bytes,
                            std::string* error)
{
  if (!total_bytes || !error) return false;
  *total_bytes = 0;
  if (!options.buffer_count || !options.chunk_bytes || options.chunk_bytes % 4096) {
    *error = "transfer ring requires buffers with page-aligned capacity";
    return false;
  }
  if (options.buffer_count > std::numeric_limits<size_t>::max() / options.chunk_bytes) {
    *error = "transfer ring memory size overflows";
    return false;
  }
  const size_t per_device = options.buffer_count * options.chunk_bytes;
  if (device_count > std::numeric_limits<size_t>::max() / per_device) {
    *error = "total transfer ring memory size overflows";
    return false;
  }
  const size_t total = per_device * device_count;
  if (memory_limit_bytes && total > memory_limit_bytes) {
    *error = "total transfer ring memory exceeds configured limit";
    return false;
  }
  *total_bytes = total;
  return true;
}

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

bool TransferBuffers::AllocationBytes(CUcontext context, const TransferOptions& options,
                                      size_t* bytes, std::string* error)
{
  if (!bytes || !error) return false;
  *bytes = 0;
  size_t logical_bytes;
  if (!ValidateTransferMemory(options, 1, 0, &logical_bytes, error)) return false;
  auto status = cuCtxSetCurrent(context);
  CUdevice device;
  CUmemAllocationProp properties{};
  size_t granularity = 0;
  if (status == CUDA_SUCCESS)
    status = HostAllocationProperties(&device, &properties, &granularity);
  if (status != CUDA_SUCCESS) {
    *error = "query transfer allocation granularity: " + CudaError(status);
    return false;
  }
  if (!granularity || options.chunk_bytes > std::numeric_limits<size_t>::max() - (granularity - 1)) {
    *error = "rounded transfer allocation size overflows";
    return false;
  }
  const size_t rounded = ((options.chunk_bytes + granularity - 1) / granularity) * granularity;
  if (options.buffer_count > std::numeric_limits<size_t>::max() / rounded) {
    *error = "rounded transfer ring memory size overflows";
    return false;
  }
  *bytes = options.buffer_count * rounded;
  return true;
}

bool TransferBuffers::Initialize(CUcontext context, std::string* error)
{
  const auto& options = impl_->options;
  size_t memory_bytes;
  if (!ValidateTransferMemory(options, 1, 0, &memory_bytes, error)) return false;
  if (!impl_->slots.empty()) {
    *error = "transfer ring is already initialized";
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
                              TransferOperation operation, TransferMetrics* metrics, std::string* error,
                              TransferControl control)
{
  *metrics = {};
  error->clear();
  const auto total_start = Clock::now();
  auto cancelled = [&]() {
    if (!control.cancellation || !control.cancellation->IsCancelled()) return false;
    *error = control.cancellation->DeadlineExceeded() ? "transfer deadline exceeded"
                                                     : "transfer cancelled";
    return true;
  };
  if (cancelled()) return false;
  if (impl_->slots.empty()) {
    *error = "transfer ring is not initialized";
    return false;
  }
  // Avoid digest construction and allocation entirely on the default fast path.
  std::unique_ptr<cuda_checkpoint_storage::ContentDigest> digest;
  if (control.enable_checksum_digest) digest = std::make_unique<cuda_checkpoint_storage::ContentDigest>();
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
  if (!size) {
    bool success = true;
    if (operation == TransferOperation::kCheckpoint && fsync(fd)) {
      *error = "sync device content: " + std::string(std::strerror(errno));
      success = false;
    }
    if (success && digest) success = digest->Finalize(&metrics->sha256, error);
    metrics->total_seconds = ElapsedSeconds(total_start);
    return success;
  }
  auto& slots = impl_->slots;
  const size_t width = slots.size();
  const size_t count = size / options.chunk_bytes + (size % options.chunk_bytes != 0);
  auto offset = [&](size_t i) { return i * options.chunk_bytes; };
  auto length = [&](size_t i) { return std::min(options.chunk_bytes, size - offset(i)); };
  auto copy = [&](size_t i) {
    if (cancelled()) return false;
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
        if (cancelled()) { success = false; break; }
        if (save) {
          if (!copy(i)) { success = false; break; }
        }
#ifdef PAGEBROKER_NIXL
        else io.Submit(i, false, offset(i), length(i));
#endif
      }
      for (size_t i = 0; success && i < count; ++i) {
        if (cancelled()) { success = false; break; }
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
        if (digest && !digest->Update(slot.data(), length(i), error)) { success = false; break; }
        if (cancelled()) { success = false; break; }
        if (save) io.Submit(index, true, offset(i), length(i));
        else {
          if (!copy(i) || !slot.Wait(metrics, error)) { success = false; break; }
          if (cancelled()) { success = false; break; }
          if (i + width < count)
            io.Submit(index, false, offset(i + width), length(i + width));
        }
#else
        if (save && digest && !digest->Update(slot.data(), length(i), error)) { success = false; break; }
        if (cancelled()) { success = false; break; }
        const auto io_start = Clock::now();
        success = PosixTransfer(save, slot.data(), fd, offset(i), length(i), error);
        metrics->storage_io_seconds += ElapsedSeconds(io_start);
        if (!success) break;
        if (!save && digest && !digest->Update(slot.data(), length(i), error)) { success = false; break; }
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
    if (success && cancelled()) success = false;
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
  if (success && digest) success = digest->Finalize(&metrics->sha256, error);
  metrics->total_seconds = ElapsedSeconds(total_start);
  if (success) metrics->bytes = size;
  return success;
}
}  // namespace snapshot::pagebroker::cuda

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "transfer.hpp"
#include "errors.hpp"
#include <exception>
#include <stdexcept>
#include <system_error>
#include <cstdio>
#include <nvtx3/nvtx3.hpp>
#include <optional>
#include <nixl.h>
#include <nixl_descriptors.h>
#include <nixl_params.h>
#include <thread>

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

// Keeps NIXL registrations and requests for the transfer ring. Wait and Close
// must finish I/O before CUDA reuses a buffer or the caller closes its file.
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
  void Wait(size_t slot);
  void Close();

 private:
  struct Request {
    nixlXferReqH* handle = nullptr;
    nixl_status_t status = NIXL_SUCCESS;
    Clock::time_point deadline;
    std::optional<nvtx3::unique_range> range;
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
  // POSIX releaseXferReq does not cancel queued AIO callbacks. Wait for I/O
  // before releasing requests or buffers.
  try {
    Close();
    CheckNixl(agent_->deregisterMem(buffers_), "deregister buffers");
  } catch (...) {
    // Keep the backend if cleanup fails. Its destructor could release
    // registrations that I/O still uses.
    (void)agent_.release();
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
  request.deadline = Clock::now() + std::chrono::seconds(240);
  request.range.emplace(write ? "PageBroker storage write" : "PageBroker storage read");
  CheckNixl(agent_->createXferReq(write ? NIXL_WRITE : NIXL_READ, local, remote,
                                  name_, request.handle), "create request");
  request.status = agent_->postXferReq(request.handle);
  if (request.status != NIXL_IN_PROG)
    CheckNixl(request.status, "post request");
}

void NixlTransfer::Wait(size_t slot)
{
  auto& request = requests_.at(slot);
  if (!request.handle) { request.range.reset(); return; }
  const nvtx3::scoped_range range{"PageBroker storage wait"};
  while (request.status == NIXL_IN_PROG) {
    request.status = agent_->getXferStatus(request.handle);
    if (request.status == NIXL_IN_PROG && Clock::now() >= request.deadline)
      throw gpu::FatalError("NIXL transfer did not drain before its deadline");
    if (request.status == NIXL_IN_PROG)
      std::this_thread::sleep_for(std::chrono::microseconds(50));
  }
  if (agent_->releaseXferReq(request.handle) != NIXL_SUCCESS)
    throw gpu::FatalError("release completed NIXL request failed");
  request.handle = nullptr;
  request.range.reset();
  CheckNixl(request.status, "transfer");
}

void NixlTransfer::Close()
{
  std::exception_ptr failure;
  for (size_t slot = 0; slot < requests_.size(); ++slot) {
    try { Wait(slot); }
    catch (const gpu::FatalError&) { throw; }
    catch (...) { if (!failure) failure = std::current_exception(); }
  }
  if (descriptor_ != -1) {
    if (agent_->deregisterMem(file_) != NIXL_SUCCESS)
      throw gpu::FatalError("NIXL file deregistration failed");
    descriptor_ = -1;
  }
  // Ordinary I/O failure does not prevent releasing every request and file.
  if (failure) std::rethrow_exception(failure);
}


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
  // Without NUMA, CUDA reports -1 and host allocations require node 0.
  properties->location.id = node == -1 ? 0 : node;
  return cuMemGetAllocationGranularity(granularity, properties, CU_MEM_ALLOC_GRANULARITY_MINIMUM);
}

class TransferSlot {
 public:
  ~TransferSlot()
  {
    // Keep memory that the GPU can still access until PageBroker exits.
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

  void Wait()
  {
    if (!pending_) return;
    const nvtx3::scoped_range range{"PageBroker CUDA wait"};
    const auto status = cuEventSynchronize(event_);
    if (status != CUDA_SUCCESS)
      throw std::runtime_error("CUDA event synchronization: " + CudaError(status));
    Complete();
  }

  void Copy(TransferOperation operation, CUdeviceptr device, size_t size, CUstream stream)
  {
    // A failed copy call can still start DMA.
    cuda_may_access_ = true;
    auto status = operation == TransferOperation::kCheckpoint
        ? cuMemcpyDtoHAsync(data(), device, size, stream)
        : cuMemcpyHtoDAsync(device, data(), size, stream);
    if (status == CUDA_SUCCESS) status = cuEventRecord(event_, stream);
    if (status != CUDA_SUCCESS)
      throw std::runtime_error("CUDA asynchronous copy: " + CudaError(status));
    pending_ = true;
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

}  // namespace

size_t TransferMemoryBytes(const TransferOptions& options, size_t device_count, size_t memory_limit_bytes)
{
  if (!options.buffer_count || !options.chunk_bytes || options.chunk_bytes % 4096)
    throw std::invalid_argument("transfer ring requires buffers with page-aligned capacity");
  if (options.buffer_count > std::numeric_limits<size_t>::max() / options.chunk_bytes)
    throw std::invalid_argument("transfer ring memory size overflows");
  const size_t per_device = options.buffer_count * options.chunk_bytes;
  if (device_count > std::numeric_limits<size_t>::max() / per_device)
    throw std::invalid_argument("total transfer ring memory size overflows");
  const size_t total = per_device * device_count;
  if (memory_limit_bytes && total > memory_limit_bytes)
    throw std::invalid_argument("total transfer ring memory exceeds configured limit");
  return total;
}

struct TransferBuffers::Impl {
  TransferOptions options;
  CUcontext context = nullptr;
  bool fatal = false;
  std::vector<std::unique_ptr<TransferSlot>> slots;
  // Destroy NIXL registrations before their pinned buffers.
  std::unique_ptr<NixlTransfer> storage;
};

TransferBuffers::TransferBuffers(TransferOptions options) : impl_(std::make_unique<Impl>())
{
  impl_->options = options;
}
TransferBuffers::~TransferBuffers()
{
  if (impl_ && impl_->fatal) (void)impl_.release();
}

size_t TransferBuffers::AllocationBytes(CUcontext context, const TransferOptions& options)
{
  TransferMemoryBytes(options, 1, 0);
  auto status = cuCtxSetCurrent(context);
  CUdevice device;
  CUmemAllocationProp properties{};
  size_t granularity = 0;
  if (status == CUDA_SUCCESS)
    status = HostAllocationProperties(&device, &properties, &granularity);
  if (status != CUDA_SUCCESS)
    throw std::runtime_error("query transfer allocation granularity: " + CudaError(status));
  if (!granularity || options.chunk_bytes > std::numeric_limits<size_t>::max() - (granularity - 1))
    throw std::invalid_argument("rounded transfer allocation size overflows");
  const size_t rounded = ((options.chunk_bytes + granularity - 1) / granularity) * granularity;
  if (options.buffer_count > std::numeric_limits<size_t>::max() / rounded)
    throw std::invalid_argument("rounded transfer ring memory size overflows");
  return options.buffer_count * rounded;
}

void TransferBuffers::Initialize(CUcontext context)
{
  const nvtx3::scoped_range range{"PageBroker transfer buffers"};
  const auto& options = impl_->options;
  TransferMemoryBytes(options, 1, 0);
  if (!impl_->slots.empty()) throw std::logic_error("transfer ring is already initialized");
  const auto status = cuCtxSetCurrent(context);
  if (status != CUDA_SUCCESS)
    throw std::runtime_error("set transfer context: " + CudaError(status));
  std::vector<std::unique_ptr<TransferSlot>> slots;
  for (size_t i = 0; i < options.buffer_count; ++i) {
    auto slot = std::make_unique<TransferSlot>();
    const auto allocated = slot->Allocate(options.chunk_bytes);
    if (allocated != CUDA_SUCCESS)
      throw std::runtime_error("allocate CUDA host NUMA buffer: " + CudaError(allocated));
    slots.push_back(std::move(slot));
  }
  std::vector<void*> addresses;
  for (const auto& slot : slots) addresses.push_back(slot->data());
  auto storage = std::make_unique<NixlTransfer>(addresses, options.chunk_bytes);
  impl_->storage = std::move(storage);
  impl_->slots = std::move(slots);
  impl_->context = context;
}

void TransferBuffers::Transfer(int fd, CUdeviceptr device, size_t size, CUstream stream,
                               TransferOperation operation, TransferControl control)
{
  const nvtx3::scoped_range range{"PageBroker extent transfer"};
  if (impl_->fatal) throw gpu::FatalError("transfer ring is unavailable after a fatal error");
  auto check_cancelled = [&] {
    if (control.cancellation && control.cancellation->IsCancelled())
      throw std::runtime_error(control.cancellation->DeadlineExceeded() ? "transfer deadline exceeded" : "transfer cancelled");
  };
  check_cancelled();
  if (impl_->slots.empty()) throw std::logic_error("transfer ring is not initialized");
  const auto status = cuCtxSetCurrent(impl_->context);
  if (status != CUDA_SUCCESS)
    throw std::runtime_error("set transfer context: " + CudaError(status));
  const auto& options = impl_->options;
  if (options.direct_io && size % 4096)
    throw std::invalid_argument("direct I/O requires a page-aligned extent");
  auto& slots = impl_->slots;
  const size_t width = slots.size();
  const size_t count = size / options.chunk_bytes + (size % options.chunk_bytes != 0);
  auto offset = [&](size_t i) { return i * options.chunk_bytes; };
  auto length = [&](size_t i) { return std::min(options.chunk_bytes, size - offset(i)); };
  auto copy = [&](size_t i) {
    check_cancelled();
    slots[i % width]->Copy(operation, device + offset(i), length(i), stream);
  };
  const bool save = operation == TransferOperation::kCheckpoint;
  if (size) {
    auto& io = *impl_->storage;
    {
      const nvtx3::scoped_range setup{"PageBroker storage setup"};
      io.Open(fd, size);
    }
    try {
      // Fill the ring, then overlap storage requests with CUDA copies.
      for (size_t i = 0; i < std::min(width, count); ++i) {
        check_cancelled();
        if (save) copy(i);
        else io.Submit(i, false, offset(i), length(i));
      }
      for (size_t i = 0; i < count; ++i) {
        check_cancelled();
        const size_t index = i % width;
        auto& slot = *slots[index];
        io.Wait(index);
        if (save && i >= width) copy(i);
        slot.Wait();
        check_cancelled();
        if (save) io.Submit(index, true, offset(i), length(i));
        else {
          copy(i);
          slot.Wait();
          check_cancelled();
          if (i + width < count) io.Submit(index, false, offset(i + width), length(i + width));
        }
      }
      for (size_t i = 0; i < width; ++i) io.Wait(i);
      io.Close();
      for (auto& slot : slots) slot->Wait();
      check_cancelled();
    } catch (const gpu::FatalError&) {
      impl_->fatal = true;
      throw;
    } catch (...) {
      try { io.Close(); }
      catch (const gpu::FatalError&) { impl_->fatal = true; throw; }
      catch (const std::exception& error) {
        std::fprintf(stderr, "drain NIXL requests: %s\n", error.what());
      }
      if (cuStreamSynchronize(stream) != CUDA_SUCCESS) {
        impl_->fatal = true;
        throw gpu::FatalError("CUDA transfer stream could not be drained");
      }
      for (auto& slot : slots) slot->Complete();
      throw;
    }
  }
  if (save) {
    const nvtx3::scoped_range sync{"PageBroker file sync"};
    if (fsync(fd)) throw std::system_error(errno, std::generic_category(), "sync GPU extent");
  }
}
}  // namespace snapshot::pagebroker::cuda

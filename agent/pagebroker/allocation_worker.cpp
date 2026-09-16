// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "allocation_transport.hpp"
#include "pagebroker_types.hpp"
#include "../cmd/cuda-checkpoint-helper/transfer_engine.hpp"

#include <cuda.h>
#include <unistd.h>
#include <chrono>
#include <cstring>
#include <iostream>
#include <stdexcept>

namespace {
using namespace snapshot::pagebroker;
namespace transfer = cuda_checkpoint_transfer;

void Check(CUresult result)
{
  if (result != CUDA_SUCCESS) {
    const char* name = nullptr;
    cuGetErrorName(result, &name);
    throw std::runtime_error(name ? name : "CUDA allocation operation failed");
  }
}

CUdevice FindDevice(const std::string& uuid)
{
  if (uuid.size() != sizeof(CUuuid))
    throw std::runtime_error("invalid allocation GPU UUID");
  int count = 0;
  Check(cuDeviceGetCount(&count));
  for (int ordinal = 0; ordinal < count; ++ordinal) {
    CUdevice device;
    CUuuid actual;
    Check(cuDeviceGet(&device, ordinal));
    Check(cuDeviceGetUuid(&actual, device));
    if (!std::memcmp(actual.bytes, uuid.data(), sizeof(actual.bytes)))
      return device;
  }
  throw std::runtime_error("allocation GPU is not visible to worker");
}

// CUDA cleanup is explicit, not a destructor: a failed synchronization must
// terminate this disposable process without unmapping potentially live DMA.
v1::AllocationSessionReply Transfer(const v1::AllocationWorkerRequest& request,
                                   const std::vector<FileDescriptor>& descriptors)
{
  const int count = request.batch().extents_size();
  if (count <= 0 || count > static_cast<int>(kAllocationBatchLimit) || descriptors.size() != 2 * size_t(count) ||
      (request.direction() != v1::BindAllocationSession::SAVE && request.direction() != v1::BindAllocationSession::LOAD))
    throw std::runtime_error("invalid allocation worker batch");
  v1::AllocationSessionReply reply;
  const auto operation = request.direction() == v1::BindAllocationSession::SAVE
      ? transfer::TransferOperation::kCheckpoint : transfer::TransferOperation::kRestore;
  // One stream/context at a time bounds pinned memory per participant. Rank
  // sessions execute concurrently; TransferExtent pipelines chunks within an
  // allocation. A scheduler can group small allocations later without changing
  // the session or transfer-backend interfaces.
  transfer::TransferCancellation cancellation(std::chrono::steady_clock::now() + std::chrono::seconds(240));
  for (int index = 0; index < count; ++index) {
    const auto& extent = request.batch().extents(index);
    if (!extent.size() || extent.size() > SIZE_MAX)
      throw std::runtime_error("invalid allocation size");
    CUdevice device = FindDevice(extent.device_uuid());
    CUcontext context;
    Check(cuDevicePrimaryCtxRetain(&context, device));
    Check(cuCtxSetCurrent(context));
    CUmemGenericAllocationHandle handle;
    Check(cuMemImportFromShareableHandle(&handle,
          reinterpret_cast<void*>(static_cast<intptr_t>(descriptors[index].get())),
          CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR));
    CUmemAllocationProp properties{};
    Check(cuMemGetAllocationPropertiesFromHandle(&properties, handle));
    if (properties.type != CU_MEM_ALLOCATION_TYPE_PINNED || properties.location.type != CU_MEM_LOCATION_TYPE_DEVICE ||
        properties.location.id != device)
      throw std::runtime_error("allocation backing does not match requested device");
    CUdeviceptr address;
    Check(cuMemAddressReserve(&address, extent.size(), 0, 0, 0));
    Check(cuMemMap(address, extent.size(), 0, handle, 0));
    CUmemAccessDesc access{{CU_MEM_LOCATION_TYPE_DEVICE, device}, CU_MEM_ACCESS_FLAGS_PROT_READWRITE};
    Check(cuMemSetAccess(address, extent.size(), &access, 1));
    CUstream stream;
    Check(cuStreamCreate(&stream, CU_STREAM_NON_BLOCKING));
    transfer::StorageLayout storage{
        {{"", static_cast<size_t>(extent.size()), descriptors[count + index].get()}},
        {{0, static_cast<size_t>(extent.size()), 0, 0}}};
    transfer::TransferMetrics metrics;
    std::string error;
    const bool success = transfer::TransferExtent(address, extent.size(), stream, context, storage, operation,
                                                  {}, &cancellation, &metrics, &error);
    // Any error exits the process; broker reaps before releasing admission.
    if (!success)
      throw std::runtime_error("allocation transfer: " + error);
    if (request.direction() == v1::BindAllocationSession::LOAD && metrics.sha256 != extent.sha256())
      throw std::runtime_error("allocation content digest mismatch");
    Check(cuStreamSynchronize(stream));
    Check(cuStreamDestroy(stream));
    Check(cuMemUnmap(address, extent.size()));
    Check(cuMemAddressFree(address, extent.size()));
    Check(cuMemRelease(handle));
    Check(cuCtxSetCurrent(nullptr));
    Check(cuDevicePrimaryCtxRelease(device));
    auto* completed = reply.mutable_completed()->add_extents();
    *completed = extent;
    completed->set_sha256(metrics.sha256);
  }
  return reply;
}
}  // namespace

int main(int argc, char** argv)
{
  if (argc != 2 || std::string_view(argv[1]) != "--socket-fd=3")
    return 2;
  try {
    Check(cuInit(0));
    int count;
    Check(cuDeviceGetCount(&count));
    if (!count)
      throw std::runtime_error("allocation worker has no visible CUDA devices");
    SetAllocationTimeout(3);
    v1::AllocationSessionReply ready;
    ready.mutable_completed();
    SendFrame(3, ready);
    for (;;) {
      v1::AllocationWorkerRequest request;
      std::vector<FileDescriptor> descriptors;
      if (!ReceiveFrame(3, request, descriptors))
        return 0;
      auto reply = Transfer(request, descriptors);
      // Export FDs themselves retain backing. Drop them before acknowledging,
      // not at the end of the next receive iteration.
      descriptors.clear();
      SendFrame(3, reply);
    }
  } catch (const std::exception& error) {
    std::cerr << "allocation worker: " << error.what() << '\n';
    // Do not acknowledge an uncertain cleanup. Process exit is the broker's
    // proof that failed worker resources cannot remain live.
    _exit(1);
  }
}

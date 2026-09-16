// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "allocation_transport.hpp"
#include "pagebroker_types.hpp"
#include "cuda_posix_transfer.hpp"
#include "../cmd/cuda-checkpoint-helper/transfer_engine.hpp"

#include <cuda.h>
#include <unistd.h>
#include <chrono>
#include <cstring>
#include <iostream>
#include <stdexcept>
#include <map>
#include <memory>
#include <sstream>
#include <future>
#include <system_error>

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

struct DeviceTransfer {
  CUdevice device;
  CUcontext context{};
  CUstream stream{};
  std::unique_ptr<transfer::TransferBuffers> buffers;

  explicit DeviceTransfer(CUdevice device) : device(device)
  {
    Check(cuDevicePrimaryCtxRetain(&context, device));
    Check(cuCtxSetCurrent(context));
    Check(cuStreamCreate(&stream, CU_STREAM_NON_BLOCKING));
    buffers = std::make_unique<transfer::TransferBuffers>(
        transfer::TransferOptions{4, transfer::kDefaultChunkBytes});
  }
  ~DeviceTransfer()
  {
    // This destructor runs only on clean worker exit. The exception boundary
    // below uses _exit so uncertain DMA never triggers resource destruction.
    if (cuCtxSetCurrent(context) != CUDA_SUCCESS || cuStreamSynchronize(stream) != CUDA_SUCCESS)
      _exit(1);
    buffers.reset();
    if (cuStreamDestroy(stream) != CUDA_SUCCESS || cuCtxSetCurrent(nullptr) != CUDA_SUCCESS ||
        cuDevicePrimaryCtxRelease(device) != CUDA_SUCCESS)
      _exit(1);
  }
};

std::map<std::string, CUdevice> Devices()
{
  std::map<std::string, CUdevice> devices;
  int count = 0;
  Check(cuDeviceGetCount(&count));
  for (int ordinal = 0; ordinal < count; ++ordinal) {
    CUdevice device;
    CUuuid actual;
    Check(cuDeviceGet(&device, ordinal));
    Check(cuDeviceGetUuid(&actual, device));
    devices.emplace(std::string(actual.bytes, sizeof(actual.bytes)), device);
  }
  return devices;
}

// CUDA cleanup is explicit, not a destructor: a failed synchronization must
// terminate this disposable process without unmapping potentially live DMA.
v1::AllocationSessionReply Transfer(const v1::AllocationWorkerRequest& request,
                                   const std::vector<FileDescriptor>& descriptors,
                                   const std::map<std::string, CUdevice>& devices,
                                   std::map<CUdevice, std::unique_ptr<DeviceTransfer>>& transfers)
{
  const int count = request.batch().extents_size();
  if (count <= 0 || count > static_cast<int>(kAllocationBatchLimit) || descriptors.size() != 2 * size_t(count) ||
      (request.direction() != v1::BindAllocationSession::SAVE && request.direction() != v1::BindAllocationSession::LOAD))
    throw std::runtime_error("invalid allocation worker batch");
  v1::AllocationSessionReply reply;
  const auto operation = request.direction() == v1::BindAllocationSession::SAVE
      ? transfer::TransferOperation::kCheckpoint : transfer::TransferOperation::kRestore;
  // Allocations are serial within a participant, with four slots overlapping
  // DMA and storage. Contexts, streams and slots survive subsequent batches.
  transfer::TransferCancellation cancellation(std::chrono::steady_clock::now() + std::chrono::seconds(240));
  for (int index = 0; index < count; ++index) {
    const auto& extent = request.batch().extents(index);
    if (!extent.size() || extent.size() > SIZE_MAX)
      throw std::runtime_error("invalid allocation size");
    const auto start = std::chrono::steady_clock::now();
    const auto found = devices.find(extent.device_uuid());
    if (found == devices.end())
      throw std::runtime_error("allocation GPU is not visible to worker");
    CUdevice device = found->second;
    auto& resources = transfers[device];
    if (!resources)
      resources = std::make_unique<DeviceTransfer>(device);
    CUcontext context = resources->context;
    CUstream stream = resources->stream;
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
    const auto mapped = std::chrono::steady_clock::now();
    transfer::StorageLayout storage{
        {{"", static_cast<size_t>(extent.size()), descriptors[count + index].get()}},
        {{0, static_cast<size_t>(extent.size()), 0, 0}}};
    transfer::TransferMetrics metrics;
    std::string error;
    const bool success = resources->buffers->Transfer(address, extent.size(), stream, context, storage, operation,
                                                      &cancellation, &metrics, &error, false);
    // Any error exits the process; broker reaps before releasing admission.
    if (!success)
      throw std::runtime_error("allocation transfer: " + error);
    Check(cuStreamSynchronize(stream));
    const auto copied = std::chrono::steady_clock::now();
    Check(cuMemUnmap(address, extent.size()));
    Check(cuMemAddressFree(address, extent.size()));
    Check(cuMemRelease(handle));
    const auto cleaned = std::chrono::steady_clock::now();
    std::ostringstream record;
    record << "allocation_transfer id=" << extent.allocation_id()
              << " direction=" << (operation == transfer::TransferOperation::kCheckpoint ? "save" : "load")
              << " bytes=" << extent.size()
              << " mapping_setup_s=" << std::chrono::duration<double>(mapped - start).count()
              << " buffer_setup_s=" << metrics.setup_seconds
              << " pipeline_s=" << metrics.pipeline_seconds
              << " cuda_wait_s=" << metrics.cuda_wait_seconds
              << " storage_io_s=" << metrics.storage_io_seconds
              << " fsync_s=" << metrics.fsync_seconds
              << " cleanup_s=" << std::chrono::duration<double>(cleaned - copied).count() << '\n';
    // One bounded write keeps independent worker records from interleaving.
    const auto line = record.str();
    const auto logged = write(STDERR_FILENO, line.data(), line.size());
    (void)logged;
    auto* completed = reply.mutable_completed()->add_extents();
    *completed = extent;
  }
  if (operation == transfer::TransferOperation::kCheckpoint) {
    // The protocol acknowledges batches, not individual extents. All files
    // must be durable before that acknowledgment, but independent fsyncs need
    // not serialize 32 network round trips. The wire batch bounds concurrency.
    const auto started = std::chrono::steady_clock::now();
    std::vector<std::future<void>> syncs;
    for (int index = 0; index < count; ++index) {
      const int fd = descriptors[count + index].get();
      syncs.push_back(std::async(std::launch::async, [fd] {
        if (fsync(fd) != 0)
          throw std::system_error(errno, std::generic_category(), "sync allocation content");
      }));
    }
    for (auto& sync : syncs)
      sync.get();
    const auto line = "allocation_batch_fsync files=" + std::to_string(count) + " seconds=" +
        std::to_string(std::chrono::duration<double>(std::chrono::steady_clock::now() - started).count()) + "\n";
    const auto logged = write(STDERR_FILENO, line.data(), line.size());
    (void)logged;
  }
  return reply;
}
}  // namespace

int main(int argc, char** argv)
{
  if (argc != 2 || std::string_view(argv[1]) != "--socket-fd=3")
    return 2;
  // Keep resources outside the catch scope: failure exits without unwinding
  // CUDA owners. Broker reaping is the proof that uncertain DMA has stopped.
  std::map<CUdevice, std::unique_ptr<DeviceTransfer>> transfers;
  try {
    Check(cuInit(0));
    int count;
    Check(cuDeviceGetCount(&count));
    if (!count)
      throw std::runtime_error("allocation worker has no visible CUDA devices");
    const auto devices = Devices();
    SetAllocationTimeout(3);
    v1::AllocationSessionReply ready;
    ready.mutable_completed();
    SendFrame(3, ready);
    for (;;) {
      v1::AllocationWorkerRequest request;
      std::vector<FileDescriptor> descriptors;
      if (!ReceiveFrame(3, request, descriptors))
        return 0;
      auto reply = Transfer(request, descriptors, devices, transfers);
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

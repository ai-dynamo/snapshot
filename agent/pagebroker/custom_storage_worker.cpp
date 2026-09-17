// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include <fcntl.h>
#include <poll.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <unistd.h>

#include <charconv>
#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <iostream>
#include <stdexcept>
#include <string>

#include "../cmd/cuda-checkpoint-helper/storage_manifest.hpp"
#include "cuda_posix_transfer.hpp"
#include "file_descriptor.hpp"

namespace transfer = cuda_checkpoint_transfer;
namespace storage = cuda_checkpoint_storage;
using Clock = std::chrono::steady_clock;

namespace {
void Check(CUresult result, const char* operation)
{
  if (result != CUDA_SUCCESS) {
    const char* name = nullptr;
    cuGetErrorName(result, &name);
    throw std::runtime_error(std::string(operation) + ": " + (name ? name : "unknown CUDA error"));
  }
}

// This qualification worker deliberately supports only one private-memory
// target on one visible GPU. The driver-owned aggregate view never crosses a
// process boundary: native preparation, PageBroker transfer and COMPLETE all
// execute here. It is not a daemon RPC endpoint.
void Run(int pid, const std::filesystem::path& directory)
{
  if (std::getenv("CUDA_CHECKPOINT_JOB_FILE"))
    throw std::runtime_error("CustomStorage qualification requires no CUDA_CHECKPOINT_JOB_FILE");
  struct stat info{};
  if (!directory.is_absolute() || lstat(directory.c_str(), &info) || !S_ISDIR(info.st_mode) ||
      (info.st_mode & 0022))
    throw std::runtime_error("expected an existing private absolute storage directory");

  const int pidfd = static_cast<int>(syscall(SYS_pidfd_open, pid, 0));
  if (pidfd < 0)
    throw std::runtime_error("pidfd_open failed");
  pollfd target{pidfd, POLLIN, 0};
  const auto admission_start = Clock::now();
  Check(cuInit(0), "cuInit");
  int count = 0;
  Check(cuDeviceGetCount(&count), "cuDeviceGetCount");
  if (count != 1)
    throw std::runtime_error("qualification worker requires exactly one visible GPU");
  CUdevice device;
  Check(cuDeviceGet(&device, 0), "cuDeviceGet");
  CUcontext context;
  Check(cuDevicePrimaryCtxRetain(&context, device), "cuDevicePrimaryCtxRetain");
  CUuuid uuid;
  Check(cuDeviceGetUuid(&uuid, device), "cuDeviceGetUuid");
  std::array<unsigned char, 16> bytes{};
  std::copy(std::begin(uuid.bytes), std::end(uuid.bytes), bytes.begin());
  const auto device_uuid = storage::FormatGPUUUID(bytes);
  void* symbol = nullptr;
  CUdriverProcAddressQueryResult query;
  Check(cuGetProcAddress("cuCheckpointOperationComplete", &symbol, 13040,
                         CU_GET_PROC_ADDRESS_DEFAULT, &query),
        "resolve COMPLETE");
  if (!symbol || query != CU_GET_PROC_ADDRESS_SUCCESS)
    throw std::runtime_error("driver does not expose CustomStorage COMPLETE");
  const auto complete = reinterpret_cast<decltype(&cuCheckpointOperationComplete)>(symbol);
  const double admission = std::chrono::duration<double>(Clock::now() - admission_start).count();
  std::printf("{\"event\":\"ready\",\"admission_seconds\":%.6f}\n", admission);
  std::fflush(stdout);
  {
    // Reuse the same pinned ring, NIXL registrations and context across SAVE
    // and LOAD. Four slots match the existing allocation worker.
    transfer::TransferBuffers buffers({4, transfer::kDefaultChunkBytes});
    std::string command;
    bool prepared = false;
    bool copied = false;
    bool save = false;
    CUcheckpointCustomStorageInfo* view = nullptr;
    std::vector<storage::ManifestExtent> manifest;
    std::vector<storage::TransferJob> jobs;
    Clock::time_point start, prepare_start, prepare_end, transfer_start, transfer_end;
    double prepare = 0, transfer_time = 0, setup_time = 0, storage_time = 0;
    size_t transferred = 0;
    while (std::getline(std::cin, command)) {
      const bool whole = command == "save" || command == "load";
      const bool begin = whole || command == "prepare-save" || command == "prepare-load";
      if (!begin && command != "transfer" && command != "complete")
        throw std::runtime_error("expected save/load, prepare-save/load, transfer or complete");
      if (poll(&target, 1, 0) != 0)
        throw std::runtime_error("target exited");
      std::string error;
      if (begin) {
        if (prepared)
          throw std::runtime_error("operation already prepared");
        save = command == "save" || command == "prepare-save";
        start = Clock::now();
        manifest.clear();
        jobs.clear();
        view = nullptr;
        copied = false;
        transferred = 0;
        setup_time = storage_time = 0;
        if (save) {
          if (!storage::RemoveManifest(directory, &error))
            throw std::runtime_error(error);
        } else if (!storage::ReadManifest(directory, &manifest, &error) ||
                   !storage::ValidateExtentFiles(directory, manifest, &error)) {
          throw std::runtime_error(error);
        }
        CUprocessState state;
        Check(cuCheckpointProcessGetState(pid, &state), "target state");
        if (state != (save ? CU_PROCESS_STATE_RUNNING : CU_PROCESS_STATE_CHECKPOINTED))
          throw std::runtime_error("target has unexpected native state");
        if (save) {
          CUcheckpointLockArgs lock{};
          lock.timeoutMs = 10000;
          Check(cuCheckpointProcessLock(pid, &lock), "native lock");
        }
        prepare_start = Clock::now();
        if (save) {
          CUcheckpointCheckpointArgs args{};
          args.customStorageInfo_out = &view;
          Check(cuCheckpointProcessCheckpoint(pid, &args), "native checkpoint prepare");
        } else {
          CUcheckpointRestoreArgs args{};
          args.customStorageInfo_out = &view;
          Check(cuCheckpointProcessRestore(pid, &args), "native restore prepare");
        }
        prepare_end = Clock::now();
        prepare = std::chrono::duration<double>(prepare_end - prepare_start).count();
        // There is no public abort after preparation. Any error below terminates
        // this worker without COMPLETE; its owner must terminate the target.
        if (!view || !view->handle || view->deviceCount > 1 ||
            (view->deviceCount && !view->perDeviceData))
          throw std::runtime_error("invalid CustomStorage view");
        std::vector<storage::DeviceExtent> extents;
        if (view->deviceCount) {
          CUcontext owner;
          Check(cuStreamGetCtx(view->perDeviceData[0].stream, &owner), "stream context");
          if (owner != context)
            throw std::runtime_error("CustomStorage stream belongs to an unexpected context");
          extents.push_back({device_uuid, view->perDeviceData[0].size});
        }
        if (save && !storage::BuildCheckpointManifest(extents, &manifest, &error))
          throw std::runtime_error(error);
        if (!storage::BuildTransferJobs(manifest, extents, {}, &jobs, &error))
          throw std::runtime_error(error);
        prepared = true;
        if (!whole) {
          std::printf(
              "{\"event\":\"prepared\",\"native_prepare_seconds\":%.6f,"
              "\"prepare_start_ns\":%lld,\"prepare_end_ns\":%lld}\n",
              prepare,
              static_cast<long long>(std::chrono::duration_cast<std::chrono::nanoseconds>(
                                         prepare_start.time_since_epoch())
                                         .count()),
              static_cast<long long>(std::chrono::duration_cast<std::chrono::nanoseconds>(
                                         prepare_end.time_since_epoch())
                                         .count()));
          std::fflush(stdout);
          continue;
        }
      }
      if (!prepared)
        throw std::runtime_error("operation is not prepared");
      if (whole || command == "transfer") {
        if (copied)
          throw std::runtime_error("operation already transferred");
        transfer_start = Clock::now();
        for (const auto& job : jobs) {
          const auto& data = view->perDeviceData[job.device_index];
          if (!data.size)
            continue;
          const auto path = directory / manifest[job.extent_index].filename;
          FileDescriptor file(
              open(path.c_str(),
                   O_CLOEXEC | O_NOFOLLOW | (save ? O_CREAT | O_TRUNC | O_RDWR : O_RDONLY), 0600));
          if (file.get() < 0 || (save && ftruncate(file.get(), data.size)))
            throw std::runtime_error("open or size CustomStorage extent failed");
          transfer::StorageLayout layout{{{path, data.size, file.get()}}, {{0, data.size, 0, 0}}};
          transfer::TransferMetrics metrics;
          if (!buffers.Transfer(data.devPtr, data.size, data.stream, context, layout,
                                save ? transfer::TransferOperation::kCheckpoint
                                     : transfer::TransferOperation::kRestore,
                                nullptr, &metrics, &error))
            throw std::runtime_error(error);
          transferred += metrics.bytes;
          setup_time += metrics.setup_seconds;
          storage_time += metrics.storage_io_seconds;
        }
        transfer_end = Clock::now();
        transfer_time = std::chrono::duration<double>(transfer_end - transfer_start).count();
        copied = true;
        if (!whole) {
          std::printf(
              "{\"event\":\"transferred\",\"bytes\":%zu,\"transfer_seconds\":%.6f,"
              "\"transfer_start_ns\":%lld,\"transfer_end_ns\":%lld}\n",
              transferred, transfer_time,
              static_cast<long long>(std::chrono::duration_cast<std::chrono::nanoseconds>(
                                         transfer_start.time_since_epoch())
                                         .count()),
              static_cast<long long>(std::chrono::duration_cast<std::chrono::nanoseconds>(
                                         transfer_end.time_since_epoch())
                                         .count()));
          std::fflush(stdout);
          continue;
        }
      }
      if (!copied)
        throw std::runtime_error("cannot COMPLETE before successful transfer");
      const auto complete_start = Clock::now();
      Check(complete(view->handle), "native COMPLETE");
      const double complete_time =
          std::chrono::duration<double>(Clock::now() - complete_start).count();
      if (save) {
        if (!storage::WriteManifest(directory, manifest, &error))
          throw std::runtime_error(error);
      } else {
        Check(cuCheckpointProcessUnlock(pid, nullptr), "native unlock");
      }
      std::printf(
          "{\"event\":\"%s\",\"bytes\":%zu,\"native_prepare_seconds\":%.6f,"
          "\"transfer_seconds\":%.6f,\"transfer_setup_seconds\":%.6f,"
          "\"storage_request_service_seconds\":%.6f,"
          "\"complete_seconds\":%.6f,\"total_seconds\":%.6f}\n",
          command.c_str(), transferred, prepare, transfer_time, setup_time, storage_time,
          complete_time, std::chrono::duration<double>(Clock::now() - start).count());
      std::fflush(stdout);
      prepared = false;
    }
    if (prepared)
      throw std::runtime_error("input closed during prepared operation");
    // Prior qualification observed target faults when a restored target's
    // retained context was released early. Keep ownership until actual exit,
    // not merely until the command pipe closes; pidfd avoids PID reuse.
    while (poll(&target, 1, -1) < 0)
      if (errno != EINTR)
        throw std::runtime_error("wait for target exit failed");
    Check(cuCtxSetCurrent(context), "cleanup context");
  }
  Check(cuDevicePrimaryCtxRelease(device), "release context");
  close(pidfd);
}
}  // namespace

int main(int argc, char** argv)
{
  try {
    int pid = 0;
    if (argc != 3)
      throw std::runtime_error("usage: pagebroker-custom-storage-worker PID DIRECTORY");
    const std::string text = argv[1];
    const auto parsed = std::from_chars(text.data(), text.data() + text.size(), pid);
    if (parsed.ec != std::errc{} || parsed.ptr != text.data() + text.size() || pid <= 0 ||
        pid == getpid())
      throw std::runtime_error("invalid target PID");
    Run(pid, argv[2]);
    return 0;
  } catch (const std::exception& error) {
    std::fprintf(stderr, "CustomStorage worker failed: %s; terminate target before cleanup\n",
                 error.what());
    // Never COMPLETE or release the retained context after an uncertain
    // native operation. The test owner must terminate the target as well.
    std::_Exit(1);
  }
}

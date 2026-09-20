// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include <fcntl.h>
#include <poll.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <unistd.h>

#include <algorithm>
#include <charconv>
#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <iostream>
#include <map>
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

int ReadTargetPID() {
  std::string identity;
  std::getline(std::cin, identity);
  int pid = 0;
  const auto parsed = std::from_chars(identity.data(), identity.data() + identity.size(), pid);
  if (parsed.ec != std::errc{} || parsed.ptr != identity.data() + identity.size() ||
      pid <= 0 || pid == getpid())
    throw std::runtime_error("invalid restored target PID");
  return pid;
}

// Aggregate pointers never cross a process boundary: native preparation,
// PageBroker transfer and COMPLETE execute here, not in a daemon RPC endpoint.
void Run(int pid, const std::filesystem::path& directory, bool jobfile_experiment, bool restore_worker)
{
  if (std::getenv("CUDA_CHECKPOINT_JOB_FILE") && !jobfile_experiment)
    throw std::runtime_error("CustomStorage qualification requires no CUDA_CHECKPOINT_JOB_FILE");
  if (jobfile_experiment && (!std::getenv("CUDA_CHECKPOINT_JOB_FILE") ||
                            !*std::getenv("CUDA_CHECKPOINT_JOB_FILE")))
    throw std::runtime_error("jobfile experiment requires a live CUDA_CHECKPOINT_JOB_FILE");
  const std::string jobfile = jobfile_experiment ? std::getenv("CUDA_CHECKPOINT_JOB_FILE") : "";
  if (jobfile_experiment && unsetenv("CUDA_CHECKPOINT_JOB_FILE"))
    throw std::runtime_error("clear helper initialization jobfile");
  struct stat info{};
  if (!directory.is_absolute() || stat(directory.c_str(), &info) || !S_ISDIR(info.st_mode) ||
      (info.st_mode & 0022))
    throw std::runtime_error("expected an existing private absolute storage directory");

  std::vector<storage::ManifestExtent> restore_manifest;
  std::string manifest_error;
  if (restore_worker && !storage::ReadManifest(directory, &restore_manifest, &manifest_error))
    throw std::runtime_error(manifest_error);
  // Empty targets have no transfer to put on the critical path. Initialize
  // their CUDA helpers when requested, keeping prewarm contention off the
  // payload workers that must become ready as CRIU finishes.
  const bool defer_initialization = !pid && restore_manifest.empty();
  if (defer_initialization) {
    std::puts("{\"event\":\"ready\",\"retained_contexts\":0,\"deferred_initialization\":true}");
    std::fflush(stdout);
    pid = ReadTargetPID();
  }

  int pidfd = pid ? static_cast<int>(syscall(SYS_pidfd_open, pid, 0)) : -1;
  if (pid && pidfd < 0)
    throw std::runtime_error("pidfd_open failed");
  const auto admission_start = Clock::now();
  Check(cuInit(0), "cuInit");
  int count = 0;
  Check(cuDeviceGetCount(&count), "cuDeviceGetCount");
  void* symbol = nullptr;
  CUdriverProcAddressQueryResult query;
  Check(cuGetProcAddress("cuCheckpointOperationComplete", &symbol, 13040,
                         CU_GET_PROC_ADDRESS_DEFAULT, &query),
        "resolve COMPLETE");
  if (!symbol || query != CU_GET_PROC_ADDRESS_SUCCESS)
    throw std::runtime_error("driver does not expose CustomStorage COMPLETE");
  const auto complete = reinterpret_cast<decltype(&cuCheckpointOperationComplete)>(symbol);
  std::vector<storage::DevicePair> device_pairs;
  std::vector<CUcheckpointGpuPair> gpu_pairs;
  if (const char* text = std::getenv("PAGEBROKER_NATIVE_DEVICE_MAP"); text && *text) {
    std::string mapping(text);
    size_t offset = 0;
    while (offset < mapping.size()) {
      const auto end = mapping.find(',', offset);
      const auto pair = mapping.substr(offset, end - offset);
      const auto equal = pair.find('=');
      std::array<unsigned char, 16> source{}, destination{};
      if (equal == std::string::npos || !storage::ParseGPUUUID(pair.substr(0, equal), &source) ||
          !storage::ParseGPUUUID(pair.substr(equal + 1), &destination))
        throw std::runtime_error("invalid native source/destination GPU map");
      CUcheckpointGpuPair gpu{};
      std::copy(source.begin(), source.end(), gpu.oldUuid.bytes);
      std::copy(destination.begin(), destination.end(), gpu.newUuid.bytes);
      gpu_pairs.push_back(gpu);
      device_pairs.push_back({pair.substr(0, equal), pair.substr(equal + 1)});
      if (end == std::string::npos) break;
      offset = end + 1;
    }
  }
  // Keep the complete CUDA enumeration for native target validation. On LOAD,
  // the manifest identifies the contexts needed for the returned mappings;
  // retaining every visible GPU needlessly initializes eight contexts per PID.
  std::map<CUcontext, std::pair<CUdevice, std::string>> contexts;
  for (int index = 0; index < count; ++index) {
    CUdevice device;
    Check(cuDeviceGet(&device, index), "cuDeviceGet");
    CUuuid uuid;
    Check(cuDeviceGetUuid(&uuid, device), "cuDeviceGetUuid");
    std::array<unsigned char, 16> bytes{};
    std::copy(std::begin(uuid.bytes), std::end(uuid.bytes), bytes.begin());
    const auto device_uuid = storage::FormatGPUUUID(bytes);
    if (restore_worker && std::none_of(restore_manifest.begin(), restore_manifest.end(),
        [&](const auto& extent) {
          const auto pair = std::find_if(device_pairs.begin(), device_pairs.end(),
              [&](const auto& value) { return value.source_uuid == extent.source_uuid; });
          return device_uuid == (pair == device_pairs.end() ? extent.source_uuid : pair->destination_uuid);
        }))
      continue;
    const char* selected = std::getenv("PAGEBROKER_NATIVE_SELECTED_GPU");
    if (selected && device_uuid != selected)
      continue;
    CUcontext context;
    Check(cuDevicePrimaryCtxRetain(&context, device), "cuDevicePrimaryCtxRetain");
    contexts.emplace(context, std::make_pair(device, device_uuid));
  }
  if (jobfile_experiment && setenv("CUDA_CHECKPOINT_JOB_FILE", jobfile.c_str(), 1))
    throw std::runtime_error("configure native operation jobfile");
  const double admission = std::chrono::duration<double>(Clock::now() - admission_start).count();
  if (!defer_initialization) {
    std::printf("{\"event\":\"ready\",\"admission_seconds\":%.6f,\"retained_contexts\":%zu}\n",
                admission, contexts.size());
    std::fflush(stdout);
  }
  if (!pid) {
    // A prewarmed LOAD has no target until CRIU finishes. Its broker supplies
    // the resolved host PID before sending the first native command.
    pid = ReadTargetPID();
    pidfd = static_cast<int>(syscall(SYS_pidfd_open, pid, 0));
    if (pidfd < 0)
      throw std::runtime_error("pidfd_open restored target failed");
  }
  pollfd target{pidfd, POLLIN, 0};
  {
    // Large LOADs need a deeper byte window to saturate NFS: eight 128-MiB
    // slots keep 1 GiB in flight. Small targets and SAVE retain the smaller ring.
    const bool large_restore = std::any_of(restore_manifest.begin(), restore_manifest.end(),
        [](const auto& extent) { return extent.size >= transfer::kMaximumPinnedBytesPerDevice; });
    transfer::TransferBuffers buffers(large_restore
        ? transfer::TransferOptions{8, 128ULL * 1024 * 1024}
        : transfer::TransferOptions{4, transfer::kDefaultChunkBytes});
    std::string command;
    bool prepared = false;
    bool copied = false;
    bool save = false;
    bool locked = false;
    CUcheckpointCustomStorageInfo* view = nullptr;
    std::vector<storage::ManifestExtent> manifest;
    std::vector<storage::TransferJob> jobs;
    std::vector<CUcontext> owners;
    Clock::time_point start, prepare_start, prepare_end, transfer_start, transfer_end;
    double prepare = 0, transfer_time = 0, setup_time = 0, storage_time = 0, cuda_wait_time = 0;
    size_t transferred = 0;
    while (std::getline(std::cin, command)) {
      if (command == "lock") {
        if (prepared || locked)
          throw std::runtime_error("target is already locked or prepared");
        CUcheckpointLockArgs lock{};
        lock.timeoutMs = 10000;
        Check(cuCheckpointProcessLock(pid, &lock), "native lock");
        locked = true;
        std::puts("{\"event\":\"locked\"}");
        std::fflush(stdout);
        continue;
      }
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
        setup_time = storage_time = cuda_wait_time = 0;
        if (save) {
          if (!storage::RemoveManifest(directory, &error))
            throw std::runtime_error(error);
        } else if (!storage::ReadManifest(directory, &manifest, &error) ||
                   !storage::ValidateExtentFiles(directory, manifest, &error)) {
          throw std::runtime_error(error);
        }
        CUprocessState state;
        Check(cuCheckpointProcessGetState(pid, &state), "target state");
        if (state != (save ? (locked ? CU_PROCESS_STATE_LOCKED : CU_PROCESS_STATE_RUNNING)
                           : CU_PROCESS_STATE_CHECKPOINTED))
          throw std::runtime_error("target has unexpected native state");
        if (save && !locked) {
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
          args.gpuPairs = gpu_pairs.data();
          args.gpuPairsCount = gpu_pairs.size();
          Check(cuCheckpointProcessRestore(pid, &args), "native restore prepare");
        }
        prepare_end = Clock::now();
        prepare = std::chrono::duration<double>(prepare_end - prepare_start).count();
        // There is no public abort after preparation. Any error below terminates
        // this worker without COMPLETE; its owner must terminate the target.
        if (!view || !view->handle || view->deviceCount > static_cast<unsigned>(count) ||
            (view->deviceCount && !view->perDeviceData))
          throw std::runtime_error("invalid CustomStorage view");
        std::vector<storage::DeviceExtent> extents;
        owners.clear();
        for (unsigned index = 0; index < view->deviceCount; ++index) {
          CUcontext owner;
          Check(cuStreamGetCtx(view->perDeviceData[index].stream, &owner), "stream context");
          const auto found = contexts.find(owner);
          if (found == contexts.end())
            throw std::runtime_error("CustomStorage stream belongs to an unexpected context");
          owners.push_back(owner);
          extents.push_back({found->second.second, view->perDeviceData[index].size});
        }
        if (save && !storage::BuildCheckpointManifest(extents, &manifest, &error))
          throw std::runtime_error(error);
        if (!storage::BuildTransferJobs(manifest, extents, device_pairs, &jobs, &error))
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
          if (!buffers.Transfer(data.devPtr, data.size, data.stream, owners[job.device_index], layout,
                                save ? transfer::TransferOperation::kCheckpoint
                                     : transfer::TransferOperation::kRestore,
                                nullptr, &metrics, &error))
            throw std::runtime_error(error);
          transferred += metrics.bytes;
          setup_time += metrics.setup_seconds;
          storage_time += metrics.storage_io_seconds;
          cuda_wait_time += metrics.cuda_wait_seconds;
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
          "\"storage_request_service_seconds\":%.6f,\"cuda_wait_seconds\":%.6f,"
          "\"complete_seconds\":%.6f,\"total_seconds\":%.6f}\n",
          command.c_str(), transferred, prepare, transfer_time, setup_time, storage_time, cuda_wait_time,
          complete_time, std::chrono::duration<double>(Clock::now() - start).count());
      std::fflush(stdout);
      prepared = false;
      locked = false;
    }
    if (prepared)
      throw std::runtime_error("input closed during prepared operation");
    // Prior qualification observed target faults when a restored target's
    // retained context was released early. Keep ownership until actual exit,
    // not merely until the command pipe closes; pidfd avoids PID reuse.
    while (poll(&target, 1, -1) < 0)
      if (errno != EINTR)
        throw std::runtime_error("wait for target exit failed");
  }
  for (const auto& [context, device] : contexts) {
    Check(cuCtxSetCurrent(context), "cleanup context");
    Check(cuDevicePrimaryCtxRelease(device.first), "release context");
  }
  close(pidfd);
}
}  // namespace

int main(int argc, char** argv)
{
  try {
    int pid = 0;
    const bool jobfile_experiment = argc == 4 && std::string(argv[3]) == "--jobfile-experiment";
    const bool restore_worker = argc == 4 && std::string(argv[3]) == "--restore";
    if (argc != 3 && !jobfile_experiment && !restore_worker)
      throw std::runtime_error("usage: pagebroker-custom-storage-worker PID DIRECTORY [--jobfile-experiment|--restore]");
    const std::string text = argv[1];
    const auto parsed = std::from_chars(text.data(), text.data() + text.size(), pid);
    if (parsed.ec != std::errc{} || parsed.ptr != text.data() + text.size() || pid < 0 || (pid == 0 && !restore_worker) ||
        pid == getpid())
      throw std::runtime_error("invalid target PID");
    Run(pid, argv[2], jobfile_experiment, restore_worker);
    return 0;
  } catch (const std::exception& error) {
    std::fprintf(stderr, "CustomStorage worker failed: %s; terminate target before cleanup\n",
                 error.what());
    // Never COMPLETE or release the retained context after an uncertain
    // native operation. The test owner must terminate the target as well.
    std::_Exit(1);
  }
}

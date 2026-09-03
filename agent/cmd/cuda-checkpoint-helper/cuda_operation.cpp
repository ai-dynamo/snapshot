/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#include "cuda_operation.h"

#include <cuda.h>
#include <fcntl.h>
#include <sys/stat.h>
#include <signal.h>
#include <unistd.h>

#include <algorithm>
#include <array>
#include <chrono>
#include <cerrno>
#include <cstdio>
#include <cstring>
#include <exception>
#include <filesystem>
#include <limits>
#include <mutex>
#include <sstream>
#include <string>
#include <thread>
#include <unordered_set>
#include <utility>
#include <vector>

#include "storage_manifest.h"
#include "transfer_config.h"
#include "transfer_engine.h"
#include "transfer_scheduler.h"

#if !defined(CUDA_VERSION) || CUDA_VERSION < 13040
#error "cuda-checkpoint-helper requires CUDA 13.4 or newer headers"
#endif

namespace cuda_checkpoint_operation {
namespace {

void PrintCudaError(CUresult status) {
  const char *name = nullptr;
  const char *message = nullptr;
  (void)cuGetErrorName(status, &name);
  (void)cuGetErrorString(status, &message);
  std::fprintf(stderr, "%s: %s\n",
               name == nullptr ? "CUDA_ERROR_UNKNOWN" : name,
               message == nullptr ? "unknown CUDA error" : message);
}

namespace storage = cuda_checkpoint_storage;
namespace transfer = cuda_checkpoint_transfer;
namespace daemon_protocol = cuda_checkpoint_daemon;
using Clock = std::chrono::steady_clock;
using OperationCompleteFn = decltype(&cuCheckpointOperationComplete);

OperationCompleteFn ResolveOperationComplete(bool *available) {
  void *symbol = nullptr;
  CUdriverProcAddressQueryResult query_status =
      CU_GET_PROC_ADDRESS_SYMBOL_NOT_FOUND;
  const CUresult status =
      cuGetProcAddress("cuCheckpointOperationComplete", &symbol, 13040,
                       CU_GET_PROC_ADDRESS_DEFAULT, &query_status);
  *available = status == CUDA_SUCCESS && symbol != nullptr &&
               query_status == CU_GET_PROC_ADDRESS_SUCCESS;
  return *available ? reinterpret_cast<OperationCompleteFn>(symbol) : nullptr;
}

double SecondsSince(Clock::time_point start) {
  return std::chrono::duration<double>(Clock::now() - start).count();
}

double SecondsBetween(Clock::time_point start, Clock::time_point end) {
  return std::chrono::duration<double>(end - start).count();
}

CUresult DeviceUUID(CUdevice device, std::string *uuid_out);

class OperationContexts {
public:
  OperationContexts() = default;
  OperationContexts(const OperationContexts &) = delete;
  OperationContexts &operator=(const OperationContexts &) = delete;

  CUresult RetainAll(int *device_count, double *enumeration_seconds,
                     double *retain_seconds) {
    const auto enumeration_start = Clock::now();
    int count = 0;
    CUresult status = cuDeviceGetCount(&count);
    *enumeration_seconds = SecondsSince(enumeration_start);
    *device_count = count;
    if (status != CUDA_SUCCESS) {
      return status;
    }

    contexts_.reserve(count);
    for (int ordinal = 0; ordinal < count; ++ordinal) {
      const auto retain_start = Clock::now();
      CUdevice device = 0;
      status = cuDeviceGet(&device, ordinal);
      if (status != CUDA_SUCCESS) {
        *retain_seconds += SecondsSince(retain_start);
        return status;
      }
      CUcontext context = nullptr;
      status = cuDevicePrimaryCtxRetain(&context, device);
      *retain_seconds += SecondsSince(retain_start);
      if (status != CUDA_SUCCESS) {
        return status;
      }
      contexts_.push_back({device, context});
    }
    return CUDA_SUCCESS;
  }

  CUresult RetainSelected(const std::vector<std::string> &selected_devices,
                          int *device_count, double *enumeration_seconds,
                          double *retain_seconds) {
    const auto enumeration_start = Clock::now();
    int count = 0;
    CUresult status = cuDeviceGetCount(&count);
    *enumeration_seconds = SecondsSince(enumeration_start);
    *device_count = count;
    if (status != CUDA_SUCCESS) {
      return status;
    }

    const std::unordered_set<std::string> selected(selected_devices.begin(),
                                                   selected_devices.end());
    contexts_.reserve(selected.size());
    for (int ordinal = 0; ordinal < count; ++ordinal) {
      CUdevice device = 0;
      status = cuDeviceGet(&device, ordinal);
      if (status != CUDA_SUCCESS) {
        return status;
      }
      std::string uuid;
      status = DeviceUUID(device, &uuid);
      if (status != CUDA_SUCCESS) {
        return status;
      }
      if (!selected.contains(uuid)) {
        continue;
      }
      const auto retain_start = Clock::now();
      CUcontext context = nullptr;
      status = cuDevicePrimaryCtxRetain(&context, device);
      *retain_seconds += SecondsSince(retain_start);
      if (status != CUDA_SUCCESS) {
        return status;
      }
      contexts_.push_back({device, context});
    }
    if (contexts_.size() != selected.size()) {
      return CUDA_ERROR_INVALID_DEVICE;
    }
    return CUDA_SUCCESS;
  }

  std::vector<CUdevice> DetachDevices() {
    std::vector<CUdevice> devices;
    devices.reserve(contexts_.size());
    for (const auto &entry : contexts_) {
      devices.push_back(entry.device);
    }
    contexts_.clear();
    return devices;
  }

  CUresult ReleaseAll() {
    CUresult first_error = CUDA_SUCCESS;
    while (!contexts_.empty()) {
      const CUresult status =
          cuDevicePrimaryCtxRelease(contexts_.back().device);
      if (first_error == CUDA_SUCCESS && status != CUDA_SUCCESS) {
        first_error = status;
      }
      contexts_.pop_back();
    }
    return first_error;
  }

  CUresult ContextAndDeviceForStream(CUstream stream, CUcontext *context_out,
                                     CUdevice *device_out) const {
    CUcontext stream_context = nullptr;
    CUresult status = cuStreamGetCtx(stream, &stream_context);
    if (status != CUDA_SUCCESS) {
      return status;
    }
    for (const auto &retained : contexts_) {
      if (retained.context == stream_context) {
        *context_out = retained.context;
        *device_out = retained.device;
        return CUDA_SUCCESS;
      }
    }
    return CUDA_ERROR_INVALID_CONTEXT;
  }

  ~OperationContexts() { (void)ReleaseAll(); }

  size_t size() const { return contexts_.size(); }

private:
  struct Entry {
    CUdevice device;
    CUcontext context;
  };
  std::vector<Entry> contexts_;
};

// CUDA 13.4 CustomStorage restore qualification found that releasing the
// helper's retained primary-context reference while the target remained alive
// could later fault that target. Keep one operation reference with the exact
// PID/start-time/cgroup identity and release it only after confirmed exit or
// PID reuse. An inconclusive /proc read must retain the reference and block new
// work rather than guessing that the target exited.
class PersistentTargetContexts {
public:
  PersistentTargetContexts() = default;
  PersistentTargetContexts(const PersistentTargetContexts &) = delete;
  PersistentTargetContexts &
  operator=(const PersistentTargetContexts &) = delete;

  CUresult Adopt(std::vector<CUdevice> devices,
                 const daemon_protocol::Request &request) {
    std::lock_guard lock(mutex_);
    for (const auto &target : targets_) {
      if (SameIdentity(target.request, request)) {
        return ReleaseDevices(devices);
      }
    }
    targets_.push_back({request, std::move(devices)});
    return CUDA_SUCCESS;
  }

  CUresult ReapExited(const std::string &proc_root,
                      std::string *identity_error) {
    std::lock_guard lock(mutex_);
    CUresult first_error = CUDA_SUCCESS;
    auto target = targets_.begin();
    while (target != targets_.end()) {
      std::string target_error;
      const auto identity_state = daemon_protocol::InspectProcessIdentity(
          target->request, proc_root, &target_error);
      if (identity_state == daemon_protocol::ProcessIdentityState::kMatches) {
        ++target;
        continue;
      }
      if (identity_state ==
          daemon_protocol::ProcessIdentityState::kIndeterminate) {
        if (identity_error != nullptr && identity_error->empty()) {
          *identity_error = "cannot safely determine target " +
                            std::to_string(target->request.pid) +
                            " identity: " + target_error;
        }
        ++target;
        continue;
      }
      const CUresult status = ReleaseDevices(target->devices);
      if (first_error == CUDA_SUCCESS && status != CUDA_SUCCESS) {
        first_error = status;
      }
      target = targets_.erase(target);
    }
    return first_error;
  }

  CUresult ReleaseAll() {
    std::lock_guard lock(mutex_);
    return ReleaseAllLocked();
  }

  CUresult TerminateAll(const std::string &proc_root,
                        std::string *identity_error) {
    std::lock_guard lock(mutex_);
    std::vector<daemon_protocol::Request> requests;
    requests.reserve(targets_.size());
    for (const auto &target : targets_) {
      requests.push_back(target.request);
    }
    if (!daemon_protocol::TerminateMatchingProcesses(
            requests, proc_root, std::chrono::seconds(5), identity_error)) {
      return CUDA_ERROR_OPERATING_SYSTEM;
    }
    return CUDA_SUCCESS;
  }

  ~PersistentTargetContexts() { (void)ReleaseAll(); }

private:
  struct TargetContexts {
    daemon_protocol::Request request;
    std::vector<CUdevice> devices;
  };

  static bool SameIdentity(const daemon_protocol::Request &left,
                           const daemon_protocol::Request &right) {
    return left.pid == right.pid &&
           left.expected_start_time_ticks == right.expected_start_time_ticks &&
           left.expected_cgroup == right.expected_cgroup;
  }

  static CUresult ReleaseDevices(const std::vector<CUdevice> &devices) {
    CUresult first_error = CUDA_SUCCESS;
    for (const CUdevice device : devices) {
      const CUresult status = cuDevicePrimaryCtxRelease(device);
      if (first_error == CUDA_SUCCESS && status != CUDA_SUCCESS) {
        first_error = status;
      }
    }
    return first_error;
  }

  CUresult ReleaseAllLocked() {
    CUresult first_error = CUDA_SUCCESS;
    for (const auto &target : targets_) {
      const CUresult status = ReleaseDevices(target.devices);
      if (first_error == CUDA_SUCCESS && status != CUDA_SUCCESS) {
        first_error = status;
      }
    }
    targets_.clear();
    return first_error;
  }

  std::mutex mutex_;
  std::vector<TargetContexts> targets_;
};

bool ParseUUID(const char *value, CUuuid *uuid_out) {
  if (value == nullptr || uuid_out == nullptr) {
    return false;
  }
  std::array<unsigned char, 16> bytes{};
  if (!storage::ParseGPUUUID(value, &bytes)) {
    return false;
  }
  static_assert(sizeof(uuid_out->bytes) == bytes.size());
  std::memcpy(uuid_out->bytes, bytes.data(), bytes.size());
  return true;
}

bool ParseDeviceMap(const std::string &device_map,
                    std::vector<CUcheckpointGpuPair> *pairs,
                    std::vector<storage::DevicePair> *storage_pairs = nullptr) {
  if (device_map.empty()) {
    return true;
  }
  std::unordered_set<std::string> source_uuids;
  std::unordered_set<std::string> destination_uuids;
  std::istringstream input(device_map);
  std::string pair;
  while (std::getline(input, pair, ',')) {
    size_t separator = pair.find('=');
    if (separator == std::string::npos ||
        pair.find('=', separator + 1) != std::string::npos) {
      return false;
    }
    CUcheckpointGpuPair parsed{};
    const std::string source_input = pair.substr(0, separator);
    const std::string destination_input = pair.substr(separator + 1);
    std::string source;
    std::string destination;
    if (!ParseUUID(source_input.c_str(), &parsed.oldUuid) ||
        !ParseUUID(destination_input.c_str(), &parsed.newUuid) ||
        !storage::CanonicalizeGPUUUID(source_input, &source) ||
        !storage::CanonicalizeGPUUUID(destination_input, &destination) ||
        !source_uuids.insert(source).second ||
        !destination_uuids.insert(destination).second) {
      return false;
    }
    pairs->push_back(parsed);
    if (storage_pairs != nullptr) {
      storage_pairs->push_back({std::move(source), std::move(destination)});
    }
  }
  return !pairs->empty();
}

bool ParseDeviceSelection(const std::string &selected_devices,
                          std::vector<std::string> *devices) {
  if (selected_devices.empty()) {
    return false;
  }
  std::unordered_set<std::string> seen;
  std::istringstream input(selected_devices);
  std::string value;
  while (std::getline(input, value, ',')) {
    std::string canonical;
    if (!storage::CanonicalizeGPUUUID(value, &canonical) ||
        !seen.insert(canonical).second) {
      return false;
    }
    devices->push_back(std::move(canonical));
  }
  return !devices->empty();
}

CUresult DeviceUUID(CUdevice device, std::string *uuid_out) {
  CUuuid uuid{};
  CUresult status = cuDeviceGetUuid(&uuid, device);
  if (status != CUDA_SUCCESS) {
    return status;
  }
  std::array<unsigned char, 16> bytes{};
  static_assert(sizeof(uuid.bytes) == bytes.size());
  std::memcpy(bytes.data(), uuid.bytes, bytes.size());
  *uuid_out = storage::FormatGPUUUID(bytes);
  return CUDA_SUCCESS;
}

struct CustomStorageResult {
  CUresult status = CUDA_SUCCESS;
  daemon_protocol::OperationState operation;
  bool fatal = false;
};

CustomStorageResult
DoCustomStorage(int pid, bool checkpoint, const std::string &device_map,
                const std::filesystem::path &storage_dir,
                const transfer::TransferOptions &transfer_options,
                Clock::time_point operation_deadline,
                Clock::time_point helper_main_start,
                OperationCompleteFn operation_complete,
                const daemon_protocol::Request *daemon_request,
                PersistentTargetContexts *persistent_contexts) {
  const auto custom_storage_start = Clock::now();
  if (operation_complete == nullptr) {
    std::fprintf(stderr, "CUDA custom storage unavailable\n");
    return {CUDA_ERROR_NOT_SUPPORTED, {}};
  }
  const auto storage_directory_start = Clock::now();
  if (!storage_dir.is_absolute()) {
    std::fprintf(stderr, "custom storage directory must be absolute\n");
    return {CUDA_ERROR_INVALID_VALUE, {}};
  }
  if (checkpoint) {
    std::error_code filesystem_error;
    std::filesystem::create_directories(storage_dir, filesystem_error);
    struct stat directory_stat{};
    if (filesystem_error || lstat(storage_dir.c_str(), &directory_stat) != 0 ||
        !S_ISDIR(directory_stat.st_mode) ||
        chmod(storage_dir.c_str(), 0700) != 0) {
      std::fprintf(stderr, "failed to create custom storage directory\n");
      return {CUDA_ERROR_OPERATING_SYSTEM, {}};
    }
    std::string remove_error;
    if (!storage::RemoveManifest(storage_dir, &remove_error)) {
      std::fprintf(stderr,
                   "failed to clear stale custom storage manifest: %s\n",
                   remove_error.c_str());
      return {CUDA_ERROR_OPERATING_SYSTEM, {}};
    }
  } else {
    struct stat directory_stat{};
    if (lstat(storage_dir.c_str(), &directory_stat) != 0 ||
        !S_ISDIR(directory_stat.st_mode) ||
        (directory_stat.st_mode & 0022) != 0) {
      std::fprintf(stderr, "custom storage directory is missing or invalid\n");
      return {CUDA_ERROR_INVALID_VALUE, {}};
    }
  }
  const double storage_directory_validation_seconds =
      SecondsSince(storage_directory_start);

  int visible_cuda_device_count = 0;
  double device_enumeration_seconds = 0.0;
  double target_context_discovery_seconds = 0.0;
  double primary_context_retain_seconds = 0.0;
  double primary_context_release_seconds = 0.0;
  OperationContexts operation_contexts;
  std::vector<std::string> selected_devices;
  if (daemon_request != nullptr &&
      !ParseDeviceSelection(daemon_request->selected_devices,
                            &selected_devices)) {
    std::fprintf(stderr, "invalid selected CUDA devices\n");
    return {CUDA_ERROR_INVALID_VALUE, {}};
  }
  // cuCheckpointProcessCheckpoint/Restore requires the helper to retain the
  // primary contexts before it can return the CustomStorage streams. Retain a
  // fresh operation reference for only the target's selected devices. A
  // successful daemon operation transfers that reference to the target-
  // identity cache; direct CLI use releases it at invocation end.
  CUresult status = CUDA_SUCCESS;
  if (selected_devices.empty()) {
    status = operation_contexts.RetainAll(
        &visible_cuda_device_count, &device_enumeration_seconds,
        &primary_context_retain_seconds);
  } else {
    status = operation_contexts.RetainSelected(
        selected_devices, &visible_cuda_device_count,
        &device_enumeration_seconds, &primary_context_retain_seconds);
  }
  if (status != CUDA_SUCCESS) {
    return {status, {}};
  }

  const auto manifest_validation_start = Clock::now();
  std::vector<storage::ManifestExtent> manifest;
  std::string manifest_error;
  if (!checkpoint &&
      (!storage::ReadManifest(storage_dir, &manifest, &manifest_error) ||
       !storage::ValidateExtentFiles(storage_dir, manifest, &manifest_error))) {
    std::fprintf(stderr, "custom storage manifest validation failed: %s\n",
                 manifest_error.c_str());
    return {CUDA_ERROR_INVALID_VALUE, {}};
  }
  const double manifest_validation_seconds =
      SecondsSince(manifest_validation_start);

  CUcheckpointCustomStorageInfo *info = nullptr;
  std::vector<CUcheckpointGpuPair> gpu_pairs;
  std::vector<storage::DevicePair> storage_pairs;
  const auto device_map_preparation_start = Clock::now();
  if (!checkpoint && !ParseDeviceMap(device_map, &gpu_pairs, &storage_pairs)) {
    return {CUDA_ERROR_INVALID_VALUE, {}};
  }
  const double device_map_preparation_seconds =
      SecondsSince(device_map_preparation_start);
  if (daemon_request != nullptr) {
    std::string identity_error;
    if (!daemon_protocol::ValidateProcessIdentity(*daemon_request, "/host/proc",
                                                  &identity_error)) {
      std::fprintf(stderr,
                   "process identity changed before CUDA operation: %s\n",
                   identity_error.c_str());
      return {CUDA_ERROR_INVALID_VALUE, {}};
    }
  }
  const auto cuda_process_api_start = Clock::now();
  if (checkpoint) {
    CUcheckpointCheckpointArgs args{};
    args.customStorageInfo_out = &info;
    status = cuCheckpointProcessCheckpoint(pid, &args);
  } else {
    CUcheckpointRestoreArgs args{};
    args.gpuPairs = gpu_pairs.empty() ? nullptr : gpu_pairs.data();
    args.gpuPairsCount = gpu_pairs.size();
    args.customStorageInfo_out = &info;
    status = cuCheckpointProcessRestore(pid, &args);
  }
  const double cuda_process_api_seconds = SecondsSince(cuda_process_api_start);
  if (status != CUDA_SUCCESS) {
    return {status, {}};
  }
  daemon_protocol::OperationState operation{.handle_returned = true};
  const auto post_handle_failure = [&operation, &operation_contexts](
                                       CUresult failure) {
    const CUresult status =
        static_cast<CUresult>(daemon_protocol::FinishHandledOperation(
            false, failure,
            [] { return static_cast<int32_t>(CUDA_SUCCESS); }, &operation));
    const CUresult release_status = operation_contexts.ReleaseAll();
    if (release_status != CUDA_SUCCESS) {
      std::fprintf(
          stderr,
          "failed to release operation CUDA contexts with status %d while "
          "handling status %d\n",
          static_cast<int>(release_status), static_cast<int>(status));
    }
    return CustomStorageResult{.status = status,
                               .operation = operation,
                               .fatal = operation.fatal() ||
                                        release_status != CUDA_SUCCESS};
  };
  const auto metadata_job_construction_start = Clock::now();
  if (info == nullptr || info->handle == nullptr ||
      info->deviceCount >
          static_cast<unsigned int>(visible_cuda_device_count) ||
      (info->deviceCount > 0 && info->perDeviceData == nullptr)) {
    std::fprintf(stderr, "CUDA returned invalid custom storage information\n");
    return post_handle_failure(CUDA_ERROR_INVALID_VALUE);
  }

  size_t pinned_bytes = 0;
  std::string transfer_config_error;
  if (!transfer::CalculatePinnedBytes(info->deviceCount, transfer_options,
                                      &pinned_bytes, &transfer_config_error)) {
    std::fprintf(stderr, "custom storage transfer configuration invalid: %s\n",
                 transfer_config_error.c_str());
    return post_handle_failure(CUDA_ERROR_INVALID_VALUE);
  }

  std::vector<CUcontext> contexts(info->deviceCount);
  std::vector<CUdevice> devices(info->deviceCount);
  std::vector<storage::DeviceExtent> device_extents;
  device_extents.reserve(info->deviceCount);
  for (unsigned int index = 0; index < info->deviceCount; ++index) {
    const auto target_context_discovery_start = Clock::now();
    status = operation_contexts.ContextAndDeviceForStream(
        info->perDeviceData[index].stream, &contexts[index], &devices[index]);
    target_context_discovery_seconds +=
        SecondsSince(target_context_discovery_start);
    if (status != CUDA_SUCCESS) {
      return post_handle_failure(status);
    }
    std::string uuid;
    status = DeviceUUID(devices[index], &uuid);
    if (status != CUDA_SUCCESS) {
      return post_handle_failure(status);
    }
    device_extents.push_back(
        {std::move(uuid), info->perDeviceData[index].size});
  }

  if (checkpoint && !storage::BuildCheckpointManifest(device_extents, &manifest,
                                                      &manifest_error)) {
    std::fprintf(stderr, "invalid checkpoint custom storage mapping: %s\n",
                 manifest_error.c_str());
    return post_handle_failure(CUDA_ERROR_INVALID_VALUE);
  }
  std::vector<storage::TransferJob> transfer_jobs;
  if (!storage::BuildTransferJobs(
          manifest, device_extents,
          checkpoint ? std::vector<storage::DevicePair>{} : storage_pairs,
          &transfer_jobs, &manifest_error)) {
    std::fprintf(stderr, "invalid restore custom storage mapping: %s\n",
                 manifest_error.c_str());
    return post_handle_failure(CUDA_ERROR_INVALID_VALUE);
  }

  size_t total_bytes = 0;
  for (const auto &extent : manifest) {
    if (extent.size > std::numeric_limits<size_t>::max() - total_bytes) {
      std::fprintf(stderr, "custom storage byte count overflow\n");
      return post_handle_failure(CUDA_ERROR_INVALID_VALUE);
    }
    total_bytes += extent.size;
  }
  const double metadata_job_construction_seconds =
      SecondsSince(metadata_job_construction_start);

  const auto start = Clock::now();
  std::vector<transfer::ScheduledTransfer> scheduled_transfers;
  size_t scheduling_job_index = 0;
  try {
    scheduled_transfers.reserve(transfer_jobs.size());
    for (; scheduling_job_index < transfer_jobs.size();
         ++scheduling_job_index) {
      const auto &job = transfer_jobs[scheduling_job_index];
      const auto &device_data = info->perDeviceData[job.device_index];
      scheduled_transfers.push_back({
          .device_ptr = device_data.devPtr,
          .extent_size = device_data.size,
          .stream = device_data.stream,
          .context = contexts[job.device_index],
          .storage = {{{storage_dir / manifest[job.extent_index].filename,
                        device_data.size}},
                      {{0, device_data.size, 0, 0}}},
          .device_index = job.device_index,
      });
    }
  } catch (const std::exception &exception) {
    const size_t device_index =
        scheduling_job_index < transfer_jobs.size()
            ? transfer_jobs[scheduling_job_index].device_index
            : scheduling_job_index;
    std::fprintf(stderr,
                 "custom storage transfer setup failed for device index %zu: "
                 "%s\n",
                 device_index, exception.what());
    return post_handle_failure(CUDA_ERROR_OPERATING_SYSTEM);
  } catch (...) {
    const size_t device_index =
        scheduling_job_index < transfer_jobs.size()
            ? transfer_jobs[scheduling_job_index].device_index
            : scheduling_job_index;
    std::fprintf(stderr,
                 "custom storage transfer setup failed for device index %zu: "
                 "unknown exception\n",
                 device_index);
    return post_handle_failure(CUDA_ERROR_OPERATING_SYSTEM);
  }
  transfer::TransferBatchResult transfer_result;
  if (!transfer::TransferBatch(
          scheduled_transfers,
          checkpoint ? transfer::TransferOperation::kCheckpoint
                     : transfer::TransferOperation::kRestore,
          transfer_options, operation_deadline, &transfer_result)) {
    std::fprintf(stderr, "%s\n", transfer_result.error.c_str());
    return post_handle_failure(CUDA_ERROR_OPERATING_SYSTEM);
  }
  const double worker_orchestration_seconds =
      transfer_result.orchestration_seconds;

  size_t transferred_bytes = 0;
  double setup_service_seconds = 0.0;
  double pipeline_service_seconds = 0.0;
  double storage_service_seconds = 0.0;
  double cuda_wait_service_seconds = 0.0;
  double fsync_service_seconds = 0.0;
  double cleanup_service_seconds = 0.0;
  for (const auto &metrics : transfer_result.metrics) {
    if (metrics.bytes >
        std::numeric_limits<size_t>::max() - transferred_bytes) {
      std::fprintf(stderr, "custom storage transferred byte count overflow\n");
      return post_handle_failure(CUDA_ERROR_OPERATING_SYSTEM);
    }
    transferred_bytes += metrics.bytes;
    setup_service_seconds += metrics.setup_seconds;
    pipeline_service_seconds += metrics.pipeline_seconds;
    storage_service_seconds += metrics.storage_seconds;
    cuda_wait_service_seconds += metrics.cuda_wait_seconds;
    fsync_service_seconds += metrics.fsync_seconds;
    cleanup_service_seconds += metrics.cleanup_seconds;
  }
  if (transferred_bytes != total_bytes) {
    std::fprintf(stderr,
                 "custom storage transfer coverage mismatch: transferred=%zu "
                 "expected=%zu\n",
                 transferred_bytes, total_bytes);
    return post_handle_failure(CUDA_ERROR_OPERATING_SYSTEM);
  }
  std::vector<std::string> extent_digests;
  extent_digests.reserve(transfer_result.metrics.size());
  for (const auto &metrics : transfer_result.metrics) {
    extent_digests.push_back(metrics.sha256);
  }
  if (!storage::ApplyOrVerifyExtentDigests(
          checkpoint, transfer_jobs, extent_digests, &manifest,
          &manifest_error)) {
    std::fprintf(stderr, "%s\n", manifest_error.c_str());
    return post_handle_failure(CUDA_ERROR_OPERATING_SYSTEM);
  }

  const auto post_transfer_validation_start = Clock::now();
  if (checkpoint) {
    if (!storage::ValidateExtentFiles(storage_dir, manifest, &manifest_error)) {
      std::fprintf(stderr, "custom storage extent validation failed: %s\n",
                   manifest_error.c_str());
      return post_handle_failure(CUDA_ERROR_OPERATING_SYSTEM);
    }
    if (!storage::WriteManifest(storage_dir, manifest, &manifest_error)) {
      std::fprintf(stderr, "custom storage manifest write failed: %s\n",
                   manifest_error.c_str());
      return post_handle_failure(CUDA_ERROR_OPERATING_SYSTEM);
    }
  }
  const double post_transfer_validation_seconds =
      SecondsSince(post_transfer_validation_start);

  // This is the sole acknowledgment point; CUDA exposes no public abort for
  // failures above.
  const auto operation_complete_start = Clock::now();
  status = static_cast<CUresult>(daemon_protocol::FinishHandledOperation(
      true, CUDA_SUCCESS,
      [operation_complete, info] {
        return static_cast<int32_t>(operation_complete(info->handle));
      },
      &operation));
  const double cuda_operation_complete_seconds =
      SecondsSince(operation_complete_start);
  if (status != CUDA_SUCCESS) {
    if (checkpoint && !storage::RemoveManifest(storage_dir, &manifest_error)) {
      std::fprintf(stderr,
                   "failed to remove custom storage manifest after CUDA "
                   "completion failure: %s\n",
                   manifest_error.c_str());
    }
    return post_handle_failure(status);
  }

  // Preserve the original transfer interval: worker setup through CUDA
  // acknowledgment.
  const double seconds = SecondsSince(start);
  const double gib_per_second = seconds == 0.0
                                    ? 0.0
                                    : static_cast<double>(total_bytes) /
                                          (1024.0 * 1024.0 * 1024.0) / seconds;
  const size_t retained_context_count = operation_contexts.size();
  const auto primary_context_release_start = Clock::now();
  const bool persist_for_target =
      daemon_request != nullptr && persistent_contexts != nullptr;
  const CUresult primary_context_release_status =
      persist_for_target
          ? persistent_contexts->Adopt(operation_contexts.DetachDevices(),
                                       *daemon_request)
          : operation_contexts.ReleaseAll();
  primary_context_release_seconds +=
      SecondsSince(primary_context_release_start);
  const char *primary_context_release_state =
      persist_for_target ? "deferred_until_target_exit" : "completed";
  const char *context_lifecycle =
      persist_for_target ? "target_identity" : "invocation";
  const auto telemetry_end = Clock::now();
  const double custom_storage_total_seconds =
      SecondsBetween(custom_storage_start, telemetry_end);
  const double helper_main_to_telemetry_seconds =
      SecondsBetween(helper_main_start, telemetry_end);
  std::fprintf(
      stdout,
      "{\"event\":\"cuda_custom_storage_transfer\",\"schema_version\":1,"
      "\"operation\":\"%s\",\"devices\":%zu,\"bytes\":%zu,"
      "\"duration_seconds\":%.6f,\"effective_gib_per_second\":%.6f,"
      "\"transfer_buffer_count\":%zu,\"transfer_chunk_bytes\":%zu,"
      "\"pinned_bytes\":%zu,\"setup_service_seconds\":%.6f,"
      "\"pipeline_service_seconds\":%.6f,\"storage_service_seconds\":%.6f,"
      "\"cuda_wait_service_seconds\":%.6f,\"fsync_service_seconds\":%.6f,"
      "\"cleanup_service_seconds\":%.6f,"
      "\"timing_scope\":\"monotonic_wall;totals_contain_subphases;"
      "service_seconds_are_cross_worker_sums_and_may_overlap\","
      "\"helper_main_to_telemetry_seconds\":%.6f,"
      "\"custom_storage_total_seconds\":%.6f,"
      "\"storage_directory_validation_seconds\":%.6f,"
      "\"cuda_device_count\":%d,"
      "\"retained_context_count\":%zu,"
      "\"device_enumeration_seconds\":%.6f,"
      "\"target_context_discovery_seconds\":%.6f,"
      "\"primary_context_retain_seconds\":%.6f,"
      "\"manifest_validation_seconds\":%.6f,"
      "\"device_map_preparation_seconds\":%.6f,"
      "\"cuda_process_api_seconds\":%.6f,"
      "\"metadata_job_construction_seconds\":%.6f,"
      "\"worker_orchestration_seconds\":%.6f,"
      "\"post_transfer_validation_seconds\":%.6f,"
      "\"cuda_operation_complete_seconds\":%.6f,"
      "\"primary_context_release_seconds\":%.6f,"
      "\"primary_context_release_state\":\"%s\","
      "\"primary_context_release_success\":%s,"
      "\"primary_context_release_status\":%d,"
      "\"context_lifecycle\":\"%s\"}\n",
      checkpoint ? "checkpoint" : "restore", manifest.size(), total_bytes,
      seconds, gib_per_second, transfer_options.buffer_count,
      transfer_options.chunk_bytes, pinned_bytes, setup_service_seconds,
      pipeline_service_seconds, storage_service_seconds,
      cuda_wait_service_seconds, fsync_service_seconds, cleanup_service_seconds,
      helper_main_to_telemetry_seconds, custom_storage_total_seconds,
      storage_directory_validation_seconds, visible_cuda_device_count,
      retained_context_count,
      device_enumeration_seconds,
      target_context_discovery_seconds, primary_context_retain_seconds,
      manifest_validation_seconds,
      device_map_preparation_seconds, cuda_process_api_seconds,
      metadata_job_construction_seconds, worker_orchestration_seconds,
      post_transfer_validation_seconds, cuda_operation_complete_seconds,
      primary_context_release_seconds, primary_context_release_state,
      primary_context_release_status == CUDA_SUCCESS ? "true" : "false",
      static_cast<int>(primary_context_release_status), context_lifecycle);
  if (primary_context_release_status != CUDA_SUCCESS) {
    std::fprintf(
        stderr,
        "warning: retained CUDA primary context release failed with status %d "
        "after operation acknowledgment\n",
        static_cast<int>(primary_context_release_status));
  }
  if (primary_context_release_status != CUDA_SUCCESS) {
    return {primary_context_release_status, operation};
  }
  return {CUDA_SUCCESS, operation, false};
}

CUresult DoRegularCheckpoint(int pid) {
  CUcheckpointCheckpointArgs args{};
  return cuCheckpointProcessCheckpoint(pid, &args);
}

CUresult DoLegacyRestore(int pid, const std::string &device_map) {
  std::vector<CUcheckpointGpuPair> pairs;
  if (!ParseDeviceMap(device_map, &pairs)) {
    return CUDA_ERROR_INVALID_VALUE;
  }
  CUcheckpointRestoreArgs args{};
  args.gpuPairs = pairs.empty() ? nullptr : pairs.data();
  args.gpuPairsCount = pairs.size();
  return cuCheckpointProcessRestore(pid, &args);
}

bool RestoreOrCloseCapturedDescriptor(int saved_fd, int target_fd) {
  if (saved_fd < 0) {
    return false;
  }
  const bool restore_failed = dup2(saved_fd, target_fd) < 0;
  close(saved_fd);
  if (!restore_failed) {
    return false;
  }

  const int null_fd = open("/dev/null", O_WRONLY | O_CLOEXEC);
  if (null_fd < 0 || dup2(null_fd, target_fd) < 0) {
    // Closing the descriptor is the last-resort guarantee that the capture
    // pipe no longer has a live writer before Finish joins its reader.
    (void)close(target_fd);
  }
  if (null_fd >= 0) {
    close(null_fd);
  }
  return true;
}

daemon_protocol::Response RunDaemonOperation(
    const daemon_protocol::Request &request,
    OperationCompleteFn operation_complete,
    std::chrono::seconds max_operation_duration,
    PersistentTargetContexts *persistent_contexts) {
  daemon_protocol::Response response;
  // Until the driver lock call begins, every failure leaves the source process
  // running. Clear this immediately before the call so callers only treat
  // failures that are known to precede CUDA mutation as safe to leave alive.
  if (request.action == daemon_protocol::Action::kLock) {
    response.flags |= daemon_protocol::kResponseLockNotAcquired;
  }
  std::string reap_error;
  const CUresult release_status =
      persistent_contexts->ReapExited("/host/proc", &reap_error);
  if (release_status != CUDA_SUCCESS) {
    response.cuda_status = release_status;
    response.flags |= daemon_protocol::kResponseFatal;
    response.error =
        "failed to release CUDA primary contexts for an exited target";
    return response;
  }
  if (!reap_error.empty()) {
    response.cuda_status = CUDA_ERROR_OPERATING_SYSTEM;
    response.error = reap_error + "; retained contexts and deferred operation";
    return response;
  }
  constexpr size_t kPerStreamCaptureLimit =
      daemon_protocol::kMaxResponseSize / 2 - 256;
  daemon_protocol::BoundedOutputCapture output_capture(
      kPerStreamCaptureLimit);
  daemon_protocol::BoundedOutputCapture error_capture(kPerStreamCaptureLimit);
  std::string capture_setup_error;
  if (!output_capture.Start(&capture_setup_error) ||
      !error_capture.Start(&capture_setup_error)) {
    response.cuda_status = CUDA_ERROR_OPERATING_SYSTEM;
    response.flags |= daemon_protocol::kResponseFatal;
    response.error = capture_setup_error;
    return response;
  }
  (void)std::fflush(stdout);
  (void)std::fflush(stderr);
  const int saved_stdout = dup(STDOUT_FILENO);
  const int saved_stderr = dup(STDERR_FILENO);
  if (saved_stdout < 0 || saved_stderr < 0 ||
      dup2(output_capture.write_fd(), STDOUT_FILENO) < 0 ||
      dup2(error_capture.write_fd(), STDERR_FILENO) < 0) {
    response.cuda_status = CUDA_ERROR_OPERATING_SYSTEM;
    response.flags |= daemon_protocol::kResponseFatal;
    response.error = "failed to redirect daemon operation output";
  } else {
    if ((!request.job_file.empty() &&
         setenv("CUDA_CHECKPOINT_JOB_FILE", request.job_file.c_str(), 1) !=
             0) ||
        (request.job_file.empty() &&
         unsetenv("CUDA_CHECKPOINT_JOB_FILE") != 0)) {
      response.cuda_status = CUDA_ERROR_OPERATING_SYSTEM;
      std::perror("configure CUDA_CHECKPOINT_JOB_FILE");
    } else if (request.backend == daemon_protocol::Backend::kPosix &&
               operation_complete == nullptr) {
      response.cuda_status = CUDA_ERROR_NOT_SUPPORTED;
      std::fprintf(
          stderr,
          "CUDA POSIX CustomStorage backend requested but the CUDA 13.4 "
          "driver API or transfer adapter is unavailable\n");
    } else if (request.action == daemon_protocol::Action::kLock ||
               request.action == daemon_protocol::Action::kUnlock) {
      std::string identity_error;
      if (!daemon_protocol::ValidateProcessIdentity(request, "/host/proc",
                                                    &identity_error)) {
        response.cuda_status = CUDA_ERROR_INVALID_VALUE;
        std::fprintf(
            stderr, "process identity changed immediately before CUDA %s: %s\n",
            daemon_protocol::ActionName(request.action),
            identity_error.c_str());
      } else if (request.action == daemon_protocol::Action::kLock) {
        CUcheckpointLockArgs lock_args{};
        std::string timeout_error;
        if (!daemon_protocol::OperationTimeoutMilliseconds(
                max_operation_duration, &lock_args.timeoutMs,
                &timeout_error)) {
          response.cuda_status = CUDA_ERROR_INVALID_VALUE;
          response.flags |= daemon_protocol::kResponseFatal;
          std::fprintf(stderr, "%s\n", timeout_error.c_str());
        } else {
          response.flags &= ~daemon_protocol::kResponseLockNotAcquired;
          response.cuda_status =
              cuCheckpointProcessLock(request.pid, &lock_args);
          if (response.cuda_status == CUDA_ERROR_NOT_READY) {
            // CUDA guarantees a timed-out lock leaves the process RUNNING.
            response.flags |= daemon_protocol::kResponseLockNotAcquired;
          }
        }
      } else {
        CUcheckpointUnlockArgs args{};
        response.cuda_status = cuCheckpointProcessUnlock(request.pid, &args);
      }
    } else if (request.backend == daemon_protocol::Backend::kRegular) {
      std::string identity_error;
      if (!daemon_protocol::ValidateProcessIdentity(request, "/host/proc",
                                                    &identity_error)) {
        response.cuda_status = CUDA_ERROR_INVALID_VALUE;
        std::fprintf(stderr,
                     "process identity changed immediately before regular "
                     "CUDA %s: %s\n",
                     daemon_protocol::ActionName(request.action),
                     identity_error.c_str());
      } else if (request.action == daemon_protocol::Action::kCheckpoint) {
        response.cuda_status = DoRegularCheckpoint(request.pid);
      } else {
        response.cuda_status = DoLegacyRestore(request.pid, request.device_map);
      }
    } else {
      transfer::TransferOptions options{
          .buffer_count = request.transfer_buffer_count,
          .chunk_bytes = static_cast<size_t>(request.transfer_chunk_bytes),
      };
      std::string validation_error;
      if (!transfer::ValidateTransferOptions(options, &validation_error)) {
        response.cuda_status = CUDA_ERROR_INVALID_VALUE;
        std::fprintf(stderr, "invalid transfer configuration: %s\n",
                     validation_error.c_str());
      } else {
        const auto operation_start = Clock::now();
        if (max_operation_duration >
            Clock::time_point::max() - operation_start) {
          response.cuda_status = CUDA_ERROR_INVALID_VALUE;
          std::fprintf(stderr,
                       "configured operation duration exceeds the steady "
                       "clock range\n");
        } else {
          const CustomStorageResult result = DoCustomStorage(
              request.pid,
              request.action == daemon_protocol::Action::kCheckpoint,
              request.device_map, request.storage_dir, options,
              operation_start + max_operation_duration, operation_start,
              operation_complete, &request, persistent_contexts);
          response.cuda_status = result.status;
          if (result.operation.fatal() || result.fatal) {
            response.flags |= daemon_protocol::kResponseFatal;
          }
        }
      }
    }
    if (response.cuda_status != CUDA_SUCCESS) {
      PrintCudaError(static_cast<CUresult>(response.cuda_status));
    }
  }
  (void)std::fflush(stdout);
  (void)std::fflush(stderr);
  const bool stdout_restore_failed =
      RestoreOrCloseCapturedDescriptor(saved_stdout, STDOUT_FILENO);
  const bool stderr_restore_failed =
      RestoreOrCloseCapturedDescriptor(saved_stderr, STDERR_FILENO);
  const bool output_restore_failed =
      stdout_restore_failed || stderr_restore_failed;
  if (output_restore_failed) {
    response.cuda_status = CUDA_ERROR_OPERATING_SYSTEM;
    response.flags |= daemon_protocol::kResponseFatal;
  }
  bool output_truncated = false;
  bool error_truncated = false;
  std::string captured_output;
  std::string captured_error;
  std::string output_capture_error;
  std::string error_capture_error;
  const bool output_finished = output_capture.Finish(
      &captured_output, &output_truncated, &output_capture_error);
  const bool error_finished = error_capture.Finish(
      &captured_error, &error_truncated, &error_capture_error);
  if (!output_finished || !error_finished) {
    response.cuda_status = CUDA_ERROR_OPERATING_SYSTEM;
    response.flags |= daemon_protocol::kResponseFatal;
    for (const std::string *capture_error : {&output_capture_error,
                                             &error_capture_error}) {
      if (capture_error->empty()) {
        continue;
      }
      if (!response.error.empty()) {
        response.error += '\n';
      }
      response.error += *capture_error;
    }
  }
  if (!captured_output.empty()) {
    if (!response.output.empty()) {
      response.output += '\n';
    }
    response.output += captured_output;
  }
  if (!captured_error.empty()) {
    if (!response.error.empty()) {
      response.error += '\n';
    }
    response.error += captured_error;
  }
  if (output_truncated) {
    response.output += "\n[stdout truncated at daemon response limit]\n";
  }
  if (error_truncated) {
    response.error += "\n[stderr truncated at daemon response limit]\n";
  }
  if (output_restore_failed) {
    if (!response.error.empty()) {
      response.error += '\n';
    }
    response.error += "failed to restore daemon output descriptors";
  }
  return response;
}


} // namespace

class Service::Impl {
public:
  explicit Impl(std::chrono::seconds max_operation_duration)
      : max_operation_duration_(max_operation_duration) {}

  bool Initialize(InitializationMetrics *metrics, std::string *error) {
    if (metrics == nullptr || error == nullptr) {
      return false;
    }
    const auto init_start = Clock::now();
    CUresult status = cuInit(0);
    metrics->cuda_init_seconds = SecondsSince(init_start);
    if (status != CUDA_SUCCESS) {
      *error = CudaError(status);
      return false;
    }

    const auto enumeration_start = Clock::now();
    status = cuDeviceGetCount(&metrics->cuda_device_count);
    metrics->device_enumeration_seconds = SecondsSince(enumeration_start);
    if (status != CUDA_SUCCESS) {
      *error = CudaError(status);
      return false;
    }
    (void)cuDriverGetVersion(&metrics->cuda_driver_version);
    operation_complete_ = ResolveOperationComplete(
        &metrics->custom_storage_driver_api_available);
    metrics->custom_storage_transfer_backend_available =
        transfer::TransferBackendAvailable();
    metrics->custom_storage_available =
        metrics->custom_storage_driver_api_available &&
        metrics->custom_storage_transfer_backend_available;
    if (!metrics->custom_storage_available) {
      operation_complete_ = nullptr;
    }
    initialized_ = true;
    return true;
  }

  daemon_protocol::Response Execute(const daemon_protocol::Request &request) {
    if (!initialized_) {
      return {.cuda_status = CUDA_ERROR_NOT_INITIALIZED,
              .flags = 0,
              .output = {},
              .error = "CUDA operation service is not initialized"};
    }
    return RunDaemonOperation(request, operation_complete_,
                              max_operation_duration_, &persistent_contexts_);
  }

  CUresult ReapExited(const std::string &proc_root,
                      std::string *identity_error) {
    return persistent_contexts_.ReapExited(proc_root, identity_error);
  }

  CUresult TerminateRetainedTargets(const std::string &proc_root,
                                    std::string *identity_error) {
    return persistent_contexts_.TerminateAll(proc_root, identity_error);
  }

  CUresult ReleaseAll() { return persistent_contexts_.ReleaseAll(); }

private:
  static std::string CudaError(CUresult status) {
    const char *name = nullptr;
    const char *message = nullptr;
    (void)cuGetErrorName(status, &name);
    (void)cuGetErrorString(status, &message);
    return std::string(name == nullptr ? "CUDA_ERROR_UNKNOWN" : name) +
           ": " +
           (message == nullptr ? "unknown CUDA error" : message);
  }

  std::chrono::seconds max_operation_duration_;
  OperationCompleteFn operation_complete_ = nullptr;
  PersistentTargetContexts persistent_contexts_;
  bool initialized_ = false;
};

Service::Service(std::chrono::seconds max_operation_duration)
    : impl_(std::make_unique<Impl>(max_operation_duration)) {}

Service::~Service() = default;

bool Service::Initialize(InitializationMetrics *metrics, std::string *error) {
  return impl_->Initialize(metrics, error);
}

daemon_protocol::Response
Service::Execute(const daemon_protocol::Request &request) {
  return impl_->Execute(request);
}

CUresult Service::ReapExited(const std::string &proc_root,
                             std::string *identity_error) {
  return impl_->ReapExited(proc_root, identity_error);
}

CUresult Service::TerminateRetainedTargets(const std::string &proc_root,
                                           std::string *identity_error) {
  return impl_->TerminateRetainedTargets(proc_root, identity_error);
}

CUresult Service::ReleaseAll() { return impl_->ReleaseAll(); }

} // namespace cuda_checkpoint_operation

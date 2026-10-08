// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "checkpoint.hpp"
#include "../errors.hpp"

#include <poll.h>
#include <signal.h>
#include <sys/syscall.h>
#include <unistd.h>
#include <cerrno>
#include <chrono>
#include <fstream>
#include <sstream>
#include <string_view>
#include <stdexcept>
#include <system_error>
#include <string>
#include <utility>

namespace snapshot::pagebroker::gpu::driver {
namespace {
constexpr int kCustomStorageCudaVersion = 13040;
constexpr auto kTargetExitTimeout = std::chrono::seconds{30};

decltype(&cuCheckpointOperationComplete)
ResolveCustomStorageCompletion()
{
  void* symbol = nullptr;
  CUdriverProcAddressQueryResult query = CU_GET_PROC_ADDRESS_SYMBOL_NOT_FOUND;
  const auto result =
      cuGetProcAddress("cuCheckpointOperationComplete", &symbol, kCustomStorageCudaVersion, CU_GET_PROC_ADDRESS_DEFAULT, &query);
  // Older drivers return INVALID_VALUE for a newer ABI. Check the version so
  // other driver errors do not cause us to report CustomStorage as unsupported.
  if (result == CUDA_ERROR_INVALID_VALUE) {
    int version = 0;
    CheckCuda(cuDriverGetVersion(&version), "cuDriverGetVersion");
    if (version < kCustomStorageCudaVersion) {
      return nullptr;
    }
  }
  if (result == CUDA_ERROR_NOT_SUPPORTED) {
    return nullptr;
  }
  CheckCuda(result, "resolve cuCheckpointOperationComplete");
  if (!symbol && (query == CU_GET_PROC_ADDRESS_SYMBOL_NOT_FOUND || query == CU_GET_PROC_ADDRESS_VERSION_NOT_SUFFICIENT)) {
    return nullptr;
  }
  if (!symbol || query != CU_GET_PROC_ADDRESS_SUCCESS) {
    throw std::runtime_error("invalid cuCheckpointOperationComplete query result");
  }
  return reinterpret_cast<decltype(&cuCheckpointOperationComplete)>(symbol);
}
} // namespace

CheckpointAPI::CheckpointAPI()
{
  const auto initialized = cuInit(0);
  if (initialized == CUDA_ERROR_NO_DEVICE) {
    return;
  }
  CheckCuda(initialized, "cuInit");
  complete_ = ResolveCustomStorageCompletion();
}

void
CheckpointAPI::RequireCustomStorage() const
{
  Require(SupportsCustomStorage(), "driver does not support CustomStorage");
}

void
ValidateTargetDescriptor(int pid, int pidfd)
{
  if (pid <= 0 || pid == getpid() || pidfd < 0) {
    throw std::invalid_argument("invalid CUDA target PID or descriptor");
  }
  const auto path = "/proc/self/fdinfo/" + std::to_string(pidfd);
  std::ifstream info(path);
  if (!info) {
    throw std::runtime_error("read GPU target fdinfo: " + path);
  }
  constexpr std::string_view kPidPrefix = "Pid:";
  int actual_pid = -1;
  bool found_pid = false;
  std::string line;
  while (std::getline(info, line)) {
    if (line.starts_with(kPidPrefix)) {
      std::istringstream value(line.substr(kPidPrefix.size()));
      if (!(value >> actual_pid) || !(value >> std::ws).eof()) {
        throw std::invalid_argument("malformed GPU target fdinfo PID");
      }
      found_pid = true;
      break;
    }
  }
  if (info.bad()) {
    throw std::runtime_error("read GPU target fdinfo: " + path);
  }
  if (!found_pid) {
    throw std::invalid_argument("GPU target fdinfo has no PID entry");
  }
  if (actual_pid != pid) {
    throw std::invalid_argument("GPU target descriptor does not match host PID");
  }
  pollfd target{pidfd, POLLIN, 0};
  int ready;
  do {
    ready = poll(&target, 1, 0);
  } while (ready < 0 && errno == EINTR);
  if (ready < 0) {
    throw std::system_error(errno, std::generic_category(), "check GPU target descriptor");
  }
  if (target.revents) {
    throw std::invalid_argument("GPU target descriptor is closed or target exited");
  }
}

Operation::Operation(const CheckpointAPI& api, int pid, FileDescriptor pidfd)
    : api_(api), pid_(pid), pidfd_(std::move(pidfd))
{
  ValidateTargetDescriptor(pid_, pidfd_.get());
}

bool
Operation::Exited() const
{
  pollfd target{pidfd_.get(), POLLIN, 0};
  int result;
  do {
    result = poll(&target, 1, 0);
  } while (result < 0 && errno == EINTR);
  if (result < 0) {
    throw std::system_error(errno, std::generic_category(), "check CUDA target");
  }
  if (target.revents & (POLLIN | POLLHUP)) {
    return true;
  }
  if (target.revents) {
    throw std::runtime_error("invalid CUDA target descriptor");
  }
  return false;
}

void
Operation::CheckTarget() const
{
  Require(!Exited(), "CUDA target exited");
}

void
Operation::Lock()
{
  CheckTarget();
  CUcheckpointLockArgs args{};
  args.timeoutMs = 10000;
  CheckCuda(cuCheckpointProcessLock(pid_, &args), "cuCheckpointProcessLock");
  locked_ = true;
}

const CUcheckpointCustomStorageInfo*
Operation::PrepareCheckpoint()
{
  CheckTarget();
  api_.RequireCustomStorage();
  prepare_started_ = true;
  CUcheckpointCheckpointArgs args{};
  args.customStorageInfo_out = &view_;
  CheckCuda(cuCheckpointProcessCheckpoint(pid_, &args), "cuCheckpointProcessCheckpoint");
  return StorageInfo();
}

const CUcheckpointCustomStorageInfo*
Operation::PrepareRestore(std::span<CUcheckpointGpuPair> pairs)
{
  CheckTarget();
  api_.RequireCustomStorage();
  prepare_started_ = true;
  CUcheckpointRestoreArgs args{};
  args.customStorageInfo_out = &view_;
  args.gpuPairs = pairs.data();
  args.gpuPairsCount = pairs.size();
  CheckCuda(cuCheckpointProcessRestore(pid_, &args), "cuCheckpointProcessRestore");
  return StorageInfo();
}

const CUcheckpointCustomStorageInfo*
Operation::StorageInfo() const
{
  Require(view_ && view_->handle && (!view_->deviceCount || view_->perDeviceData),
          "invalid CustomStorage view");
  return view_;
}

void
Operation::Complete()
{
  if (completion_failed_) {
    throw FatalError("cuCheckpointOperationComplete previously failed");
  }
  if (!view_) {
    return;
  }
  const auto handle = std::exchange(view_, nullptr)->handle;
  if (!handle) {
    completion_failed_ = true;
    throw FatalError("CustomStorage returned no completion handle");
  }
  const auto result = api_.complete_(handle);
  if (result != CUDA_SUCCESS) {
    // The driver may have freed the view even on failure. Never reuse the
    // pointer or handle. PageBroker must exit to release any remaining mappings.
    completion_failed_ = true;
    throw FatalError(CudaErrorMessage(result, "cuCheckpointOperationComplete"));
  }
}

void
Operation::Unlock()
{
  CheckTarget();
  if (completion_failed_) {
    throw FatalError("cannot unlock after cuCheckpointOperationComplete failed");
  }
  CUcheckpointUnlockArgs args{};
  const auto result = cuCheckpointProcessUnlock(pid_, &args);
  if (result != CUDA_SUCCESS) {
    // Unlock can report an error after the target has resumed. Accept it only
    // when the driver confirms that the target is running.
    CUprocessState state;
    if (cuCheckpointProcessGetState(pid_, &state) == CUDA_SUCCESS && state == CU_PROCESS_STATE_RUNNING) {
      locked_ = false;
      return;
    }
  }
  CheckCuda(result, "cuCheckpointProcessUnlock");
  locked_ = false;
}

void
Operation::Abort()
{
  if (!prepare_started_) {
    if (locked_ && !Exited()) {
      try {
        Unlock();
      } catch (const FatalError&) {
        throw;
      } catch (const std::runtime_error&) {
        if (!Exited()) {
          throw;
        }
      }
    }
    return;
  }
  if (syscall(SYS_pidfd_send_signal, pidfd_.get(), SIGKILL, nullptr, 0) && errno != ESRCH) {
    throw std::system_error(errno, std::generic_category(), "kill CUDA target");
  }
  WaitForTargetExit();
}

void
Operation::WaitForTargetExit() const
{
  pollfd target{pidfd_.get(), POLLIN, 0};
  const auto deadline = std::chrono::steady_clock::now() + kTargetExitTimeout;
  for (;;) {
    const auto remaining = deadline - std::chrono::steady_clock::now();
    if (remaining <= std::chrono::steady_clock::duration::zero()) {
      throw FatalError("CUDA target did not exit before cleanup deadline");
    }
    const auto timeout = std::chrono::ceil<std::chrono::milliseconds>(remaining).count();
    const auto status = poll(&target, 1, static_cast<int>(timeout));
    if (status < 0 && errno == EINTR) {
      continue;
    }
    if (status < 0) {
      throw FatalError(std::system_error(errno, std::generic_category(), "wait for CUDA target exit").what());
    }
    if (status == 0) {
      throw FatalError("CUDA target did not exit before cleanup deadline");
    }
    if (target.revents & (POLLIN | POLLHUP)) {
      return;
    }
    throw FatalError("invalid CUDA target descriptor while waiting for exit");
  }
}

} // namespace snapshot::pagebroker::gpu::driver

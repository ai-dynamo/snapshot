// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "checkpoint.hpp"

#include <poll.h>
#include <signal.h>
#include <sys/syscall.h>
#include <unistd.h>
#include <cerrno>
#include <stdexcept>
#include <system_error>
#include <string>
#include <utility>

namespace snapshot::pagebroker::gpu::driver {
namespace {
void
Check(CUresult result, const char* operation)
{
  if (result == CUDA_SUCCESS)
    return;
  const char* name = nullptr;
  cuGetErrorName(result, &name);
  throw std::runtime_error(std::string(operation) + ": " + (name ? name : "unknown CUDA error"));
}
} // namespace

CheckpointAPI::CheckpointAPI()
{
  const auto initialized = cuInit(0);
  if (initialized == CUDA_ERROR_NO_DEVICE) return;
  Check(initialized, "cuInit");
  void* symbol = nullptr;
  CUdriverProcAddressQueryResult query = CU_GET_PROC_ADDRESS_SYMBOL_NOT_FOUND;
  const auto result =
      cuGetProcAddress("cuCheckpointOperationComplete", &symbol, 13040, CU_GET_PROC_ADDRESS_DEFAULT, &query);
  // Older drivers return INVALID_VALUE for a newer ABI. Check the version so
  // other driver errors do not cause us to report CustomStorage as unsupported.
  if (result == CUDA_ERROR_INVALID_VALUE) {
    int version = 0;
    Check(cuDriverGetVersion(&version), "cuDriverGetVersion");
    if (version < 13040)
      return;
  }
  if (result == CUDA_ERROR_NOT_SUPPORTED)
    return;
  Check(result, "resolve cuCheckpointOperationComplete");
  if (!symbol && (query == CU_GET_PROC_ADDRESS_SYMBOL_NOT_FOUND || query == CU_GET_PROC_ADDRESS_VERSION_NOT_SUFFICIENT))
    return;
  if (!symbol || query != CU_GET_PROC_ADDRESS_SUCCESS)
    throw std::runtime_error("invalid cuCheckpointOperationComplete query result");
  complete_ = reinterpret_cast<decltype(complete_)>(symbol);
}

void
CheckpointAPI::RequireCustomStorage() const
{
  if (!SupportsCustomStorage())
    throw std::runtime_error("driver does not support CustomStorage");
}

Operation::Operation(const CheckpointAPI& api, int pid) : api_(api), pid_(pid)
{
  if (pid <= 0 || pid == getpid())
    throw std::runtime_error("invalid CUDA target PID");
  pidfd_ = static_cast<int>(syscall(SYS_pidfd_open, pid, 0));
  if (pidfd_ < 0)
    throw std::runtime_error("open CUDA target pidfd");
}

Operation::~Operation()
{
  close(pidfd_);
}

void
Operation::CheckTarget() const
{
  pollfd target{pidfd_, POLLIN, 0};
  int result;
  do {
    result = poll(&target, 1, 0);
  } while (result < 0 && errno == EINTR);
  if (result < 0)
    throw std::system_error(errno, std::generic_category(), "check CUDA target");
  if (target.revents & POLLIN)
    throw std::runtime_error("CUDA target exited");
  if (target.revents)
    throw std::runtime_error("invalid CUDA target descriptor");
}

void
Operation::Lock()
{
  touched_ = true;
  CUcheckpointLockArgs args{};
  args.timeoutMs = 10000;
  Check(cuCheckpointProcessLock(pid_, &args), "cuCheckpointProcessLock");
}

const CUcheckpointCustomStorageInfo*
Operation::Prepare(bool save, std::span<CUcheckpointGpuPair> pairs)
{
  api_.RequireCustomStorage();
  touched_ = true;
  if (save) {
    CUcheckpointCheckpointArgs args{};
    args.customStorageInfo_out = &view_;
    Check(cuCheckpointProcessCheckpoint(pid_, &args), "cuCheckpointProcessCheckpoint");
  } else {
    CUcheckpointRestoreArgs args{};
    args.customStorageInfo_out = &view_;
    args.gpuPairs = pairs.data();
    args.gpuPairsCount = pairs.size();
    Check(cuCheckpointProcessRestore(pid_, &args), "cuCheckpointProcessRestore");
  }
  if (!view_ || !view_->handle || (view_->deviceCount && !view_->perDeviceData))
    throw std::runtime_error("invalid CustomStorage view");
  return view_;
}

void
Operation::Complete()
{
  if (completion_failed_)
    throw FatalError("cuCheckpointOperationComplete previously failed");
  if (!view_)
    return;
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
    const char* name = nullptr;
    cuGetErrorName(result, &name);
    throw FatalError(std::string("cuCheckpointOperationComplete: ") + (name ? name : "unknown CUDA error"));
  }
}

void
Operation::Unlock()
{
  if (completion_failed_)
    throw FatalError("cannot unlock after cuCheckpointOperationComplete failed");
  CUcheckpointUnlockArgs args{};
  const auto result = cuCheckpointProcessUnlock(pid_, &args);
  if (result != CUDA_SUCCESS) {
    // Unlock can report an error after the target has resumed. Accept it only
    // when the driver confirms that the target is running.
    CUprocessState state;
    if (cuCheckpointProcessGetState(pid_, &state) == CUDA_SUCCESS && state == CU_PROCESS_STATE_RUNNING)
      return;
  }
  Check(result, "cuCheckpointProcessUnlock");
}

void
Operation::Terminate()
{
  if (touched_) {
    if (syscall(SYS_pidfd_send_signal, pidfd_, SIGKILL, nullptr, 0) && errno != ESRCH)
      throw std::runtime_error("kill CUDA target");
    pollfd target{pidfd_, POLLIN, 0};
    int status;
    do {
      status = poll(&target, 1, 30000);
    } while (status < 0 && errno == EINTR);
    if (status <= 0)
      throw FatalError("CUDA target did not exit before cleanup deadline");
  }
}

} // namespace snapshot::pagebroker::gpu::driver

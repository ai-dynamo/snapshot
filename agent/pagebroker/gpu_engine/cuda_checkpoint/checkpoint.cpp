// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "checkpoint.hpp"
#include "driver_ops.h"

#include <poll.h>
#include <signal.h>
#include <sys/syscall.h>
#include <unistd.h>
#include <cerrno>
#include <cstdio>
#include <stdexcept>
#include <string>
#include <utility>

namespace snapshot::cuda_checkpoint {
namespace {
void Check(CUresult result, const char* operation) {
  if (result == CUDA_SUCCESS) return;
  const char* name = nullptr;
  cuGetErrorName(result, &name);
  throw std::runtime_error(std::string(operation) + ": " + (name ? name : "unknown CUDA error"));
}
}  // namespace

CheckpointAPI::CheckpointAPI() { Check(cuInit(0), "cuInit"); }

void CheckpointAPI::RequireCustomStorage() const {
  std::call_once(completion_once_, [this] {
    void* symbol = nullptr;
    CUdriverProcAddressQueryResult query;
    Check(cuGetProcAddress("cuCheckpointOperationComplete", &symbol, 13040,
                          CU_GET_PROC_ADDRESS_DEFAULT, &query), "resolve COMPLETE");
    if (!symbol || query != CU_GET_PROC_ADDRESS_SUCCESS)
      throw std::runtime_error("driver does not expose CustomStorage COMPLETE");
    complete_ = reinterpret_cast<decltype(complete_)>(symbol);
  });
}

Operation::Operation(const CheckpointAPI& api, int pid)
    : api_(api), pid_(pid) {
  if (pid <= 0 || pid == getpid()) throw std::runtime_error("invalid native target");
  pidfd_ = static_cast<int>(syscall(SYS_pidfd_open, pid, 0));
  if (pidfd_ < 0) throw std::runtime_error("pin native target");
}

Operation::~Operation() { close(pidfd_); }

void Operation::CheckTarget() const {
  pollfd target{pidfd_, POLLIN, 0};
  if (poll(&target, 1, 0) != 0) throw std::runtime_error("native target exited");
}

void Operation::Lock() {
  touched_ = true;
  Check(snapshot_cuda_lock(pid_, 10000), "native lock");
}

const CUcheckpointCustomStorageInfo* Operation::Prepare(bool save, std::span<CUcheckpointGpuPair> pairs,
                                                       bool custom_storage) {
  // Resolve optional APIs before touching the target. Ordinary driver-managed
  // operations must also work on drivers without CustomStorage completion.
  if (custom_storage) api_.RequireCustomStorage();
  touched_ = true;
  if (save) {
    CUcheckpointCheckpointArgs args{};
    args.customStorageInfo_out = custom_storage ? &view_ : nullptr;
    Check(snapshot_cuda_checkpoint(pid_, &args), "native checkpoint prepare");
  } else {
    CUcheckpointRestoreArgs args{};
    args.customStorageInfo_out = custom_storage ? &view_ : nullptr;
    args.gpuPairs = pairs.data();
    args.gpuPairsCount = pairs.size();
    Check(snapshot_cuda_restore(pid_, &args), "native restore prepare");
  }
  if (custom_storage && (!view_ || !view_->handle || (view_->deviceCount && !view_->perDeviceData)))
    throw std::runtime_error("invalid CustomStorage view");
  return view_;
}

void Operation::Complete() {
  if (completion_uncertain_) throw CompletionUncertain("native COMPLETE has uncertain mapping lifetime");
  // Driver-managed checkpoint/restore completes inside Prepare.
  if (!view_) return;
  const auto handle = std::exchange(view_, nullptr)->handle;
  const auto result = api_.complete_(handle);
  if (result != CUDA_SUCCESS) {
    // The driver may have freed view even on failure; never retry this pointer
    // or handle. Only worker-process teardown can release uncertain mappings.
    completion_uncertain_ = true;
    const char* name = nullptr;
    cuGetErrorName(result, &name);
    throw CompletionUncertain(std::string("native COMPLETE: ") + (name ? name : "unknown CUDA error"));
  }
}

void Operation::Unlock() {
  if (completion_uncertain_) throw CompletionUncertain("cannot unlock after failed native COMPLETE");
  Check(snapshot_cuda_unlock(pid_), "native unlock");
}

void Operation::Abort() {
  // No public abort exists. The session has drained I/O and DMA; terminate the
  // pinned target, then release caller-side streams and imported mappings.
  if (touched_) {
    if (syscall(SYS_pidfd_send_signal, pidfd_, SIGKILL, nullptr, 0) && errno != ESRCH)
      throw std::runtime_error("terminate cancelled native target");
    pollfd target{pidfd_, POLLIN, 0};
    int status;
    do { status = poll(&target, 1, -1); } while (status < 0 && errno == EINTR);
    if (status <= 0) throw std::runtime_error("wait for cancelled target");
  }
  if (completion_uncertain_)
    throw CompletionUncertain("failed native COMPLETE requires engine process teardown");
  if (view_) {
    const auto handle = std::exchange(view_, nullptr)->handle;
    // The qualified driver destroys the operation even when the dead target
    // rejects completion. Persistent contexts remain owned by the GPU worker.
    const auto result = api_.complete_(handle);
    std::fprintf(stderr, "Cancelled native target=%d COMPLETE result=%d; mappings drained\n", pid_, result);
  }
}
}  // namespace snapshot::cuda_checkpoint

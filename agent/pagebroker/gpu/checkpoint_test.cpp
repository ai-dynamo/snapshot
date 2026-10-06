// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// Run on a GPU host with a CustomStorage-capable driver. All CUDA calls use
// the real driver. The child owns the allocation being checkpointed.
#include "checkpoint.hpp"
#include "../file_descriptor.hpp"

#include <gtest/gtest.h>
#include <algorithm>
#include <cerrno>
#include <cstdio>
#include <cstdlib>
#include <csignal>
#include <memory>
#include <stdexcept>
#include <string>
#include <sys/wait.h>
#include <unistd.h>
#include <vector>

namespace {
using snapshot::pagebroker::gpu::driver::CheckpointAPI;
using snapshot::pagebroker::gpu::driver::Operation;

void Check(CUresult result, const char* action)
{
  if (result == CUDA_SUCCESS) return;
  const char* name = nullptr;
  cuGetErrorName(result, &name);
  throw std::runtime_error(std::string(action) + ": " + (name ? name : "unknown CUDA error"));
}

void Signal(int fd)
{
  const char value = 1;
  ssize_t result;
  do { result = write(fd, &value, 1); } while (result < 0 && errno == EINTR);
  if (result != 1) throw std::runtime_error("write GPU test signal");
}

void Wait(int fd)
{
  char value;
  ssize_t result;
  do { result = read(fd, &value, 1); } while (result < 0 && errno == EINTR);
  if (result != 1) throw std::runtime_error("GPU test peer exited");
}

void Workload(int ready, int resume)
{
  Check(cuInit(0), "initialize workload CUDA");
  CUdevice device;
  CUcontext context;
  Check(cuDeviceGet(&device, 0), "get workload device");
  Check(cuDevicePrimaryCtxRetain(&context, device), "retain workload context");
  Check(cuCtxSetCurrent(context), "select workload context");
  constexpr size_t bytes = 4 * 1024 * 1024;
  constexpr unsigned char pattern = 0xa5;
  CUdeviceptr allocation;
  Check(cuMemAlloc(&allocation, bytes), "allocate workload memory");
  Check(cuMemsetD8(allocation, pattern, bytes), "write workload pattern");
  Check(cuCtxSynchronize(), "finish workload writes");
  Signal(ready);
  Wait(resume);
  std::vector<unsigned char> actual(bytes);
  Check(cuMemcpyDtoH(actual.data(), allocation, bytes), "read restored memory");
  if (!std::all_of(actual.begin(), actual.end(), [](auto value) { return value == pattern; }))
    throw std::runtime_error("restored GPU bytes differ");
  Check(cuMemFree(allocation), "free workload memory");
  Check(cuDevicePrimaryCtxRelease(device), "release workload context");
}

class Target {
 public:
  Target()
  {
    int ready[2], resume[2];
    if (pipe(ready)) throw std::runtime_error("create GPU test readiness pipe");
    FileDescriptor ready_read(ready[0]), ready_write(ready[1]);
    if (pipe(resume)) throw std::runtime_error("create GPU test resume pipe");
    FileDescriptor resume_read(resume[0]), resume_write(resume[1]);
    pid = fork();
    if (pid < 0) throw std::runtime_error("fork GPU test workload");
    if (pid == 0) {
      ready_read = FileDescriptor(-1);
      resume_write = FileDescriptor(-1);
      try { Workload(ready_write.get(), resume_read.get()); }
      catch (const std::exception& error) {
        std::fprintf(stderr, "%s\n", error.what());
        std::_Exit(1);
      }
      std::_Exit(0);
    }
    ready_ = std::move(ready_read);
    resume_ = std::move(resume_write);
  }
  ~Target()
  {
    if (pid > 0) {
      kill(pid, SIGKILL);
      while (waitpid(pid, nullptr, 0) < 0 && errno == EINTR) {}
    }
  }
  void Ready() { Wait(ready_.get()); }
  int Verify()
  {
    Signal(resume_.get());
    int status;
    pid_t result;
    do { result = waitpid(pid, &status, 0); } while (result < 0 && errno == EINTR);
    if (result < 0) throw std::runtime_error("wait for GPU test workload");
    pid = -1;
    return WIFEXITED(status) ? WEXITSTATUS(status) : -1;
  }
  int pid = -1;
 private:
  FileDescriptor ready_{-1}, resume_{-1};
};

struct HostMemory {
  void operator()(unsigned char* memory) const { if (memory) cuMemFreeHost(memory); }
};
using Buffer = std::unique_ptr<unsigned char, HostMemory>;

void SelectContext(CUstream stream)
{
  CUcontext context;
  Check(cuStreamGetCtx(stream, &context), "get checkpoint stream context");
  Check(cuCtxSetCurrent(context), "select checkpoint stream context");
}

TEST(GpuCheckpoint, RestoresAllocationThroughRealCustomStorage)
{
  // Fork before the parent initializes CUDA.
  Target target;
  target.Ready();
  CheckpointAPI api;
  api.RequireCustomStorage();
  CUdevice device;
  CUcontext context;
  Check(cuDeviceGet(&device, 0), "get checkpoint device");
  Check(cuDevicePrimaryCtxRetain(&context, device), "retain checkpoint context");
  Check(cuCtxSetCurrent(context), "select checkpoint context");
  for (int iteration = 0; iteration < 2; ++iteration) {
    Operation checkpoint(api, target.pid);
    checkpoint.Lock();
    const auto* saved = checkpoint.Prepare(true, {});
    std::vector<Buffer> buffers;
    std::vector<size_t> sizes;
    for (unsigned int i = 0; i < saved->deviceCount; ++i) {
      const auto& data = saved->perDeviceData[i];
      void* memory = nullptr;
      if (data.size) {
        SelectContext(data.stream);
        Check(cuMemHostAlloc(&memory, data.size, CU_MEMHOSTALLOC_PORTABLE), "allocate checkpoint buffer");
      }
      buffers.emplace_back(static_cast<unsigned char*>(memory));
      sizes.push_back(data.size);
      if (data.size) {
        Check(cuMemcpyDtoHAsync(memory, data.devPtr, data.size, data.stream), "save checkpoint bytes");
        Check(cuStreamSynchronize(data.stream), "finish checkpoint read");
      }
    }
    checkpoint.Complete();
    Operation restore(api, target.pid);
    const auto* restored = restore.Prepare(false, {});
    ASSERT_EQ(restored->deviceCount, buffers.size());
    for (unsigned int i = 0; i < restored->deviceCount; ++i) {
      const auto& data = restored->perDeviceData[i];
      ASSERT_EQ(data.size, sizes[i]);
      if (!data.size) continue;
      SelectContext(data.stream);
      Check(cuMemcpyHtoDAsync(data.devPtr, buffers[i].get(), data.size, data.stream), "restore checkpoint bytes");
      Check(cuStreamSynchronize(data.stream), "finish checkpoint write");
    }
    restore.Complete();
    restore.Unlock();
  }
  EXPECT_EQ(target.Verify(), 0);
  Check(cuDevicePrimaryCtxRelease(device), "release checkpoint context");
}
}  // namespace

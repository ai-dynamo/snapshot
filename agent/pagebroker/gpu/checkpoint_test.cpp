// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// Run on a GPU host with a CustomStorage-capable driver. All CUDA calls use
// the real driver. The child owns the allocation being checkpointed.
#include "checkpoint.hpp"
#include "../file_descriptor.hpp"
#include "../broker.hpp"
#include "../test_helpers.hpp"
#include "storage_manifest.hpp"

#include <gtest/gtest.h>
#include <algorithm>
#include <array>
#include <cerrno>
#include <chrono>
#include <condition_variable>
#include <filesystem>
#include <fstream>
#include <future>
#include <cstdio>
#include <cstdlib>
#include <csignal>
#include <fcntl.h>
#include <memory>
#include <limits>
#include <mutex>
#include <poll.h>
#include <stdexcept>
#include <system_error>
#include <string>
#include <sys/wait.h>
#include <sys/syscall.h>
#include <unistd.h>
#include <utility>
#include <vector>

namespace {
using snapshot::pagebroker::gpu::driver::CheckpointAPI;
using snapshot::pagebroker::gpu::driver::Operation;

constexpr auto kGpuTestWait = std::chrono::seconds{30};
constexpr auto kAbortResponseWait = std::chrono::seconds{35};
constexpr auto kExpiryAdvance = std::chrono::hours{3};

// Test synchronization only. The wrappers below call the real CUDA functions.
// A copy pause holds stream work, while an unlock pause holds its completed
// call's return. Neither changes CUDA results or substitutes an engine.
class CudaPause {
 public:
  void Arm()
  {
    std::lock_guard lock(mutex_);
    armed_ = true;
    reached_ = false;
    released_ = false;
  }

  bool Claim()
  {
    std::lock_guard lock(mutex_);
    return std::exchange(armed_, false);
  }

  void Arrive(CUresult result, CUstream stream = nullptr)
  {
    std::lock_guard lock(mutex_);
    result_ = result;
    stream_ = stream;
    reached_ = true;
    changed_.notify_all();
  }

  bool Wait()
  {
    std::unique_lock lock(mutex_);
    return changed_.wait_for(lock, kGpuTestWait, [&] { return reached_; });
  }

  void Hold()
  {
    std::unique_lock lock(mutex_);
    changed_.wait(lock, [&] { return released_; });
  }

  void Release()
  {
    std::lock_guard lock(mutex_);
    armed_ = false;
    released_ = true;
    changed_.notify_all();
  }

  // Read after Wait, which synchronizes with Arrive.
  CUresult result_ = CUDA_ERROR_NOT_READY;
  CUstream stream_ = nullptr;

 private:
  std::mutex mutex_;
  std::condition_variable changed_;
  bool armed_ = false;
  bool reached_ = false;
  bool released_ = false;
};

CudaPause copy_pause;
CudaPause unlock_pause;

class ScopedPause {
 public:
  explicit ScopedPause(CudaPause& pause) : pause_(pause)
  {
    pause_.Arm();
  }
  ~ScopedPause()
  {
    pause_.Release();
  }
 private:
  CudaPause& pause_;
};

FileDescriptor OpenPidfd(int pid)
{
  FileDescriptor descriptor(static_cast<int>(syscall(SYS_pidfd_open, pid, 0)));
  if (descriptor.get() < 0) {
    throw std::system_error(errno, std::generic_category(), "open test pidfd");
  }
  return descriptor;
}


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

struct RestoreCase {
  snapshot::pagebroker::Request request;
  std::filesystem::path staging;
  std::filesystem::path source;
};

TEST(GpuCheckpoint, RestoresAllocationThroughRealCustomStorage)
{
  // Fork before the parent initializes CUDA.
  Target target;
  Target next_target;
  // Broker failure cases also fork before the parent initializes CUDA.
  Target cancelled_target;
  Target expired_target;
  Target late_target;
  target.Ready();
  next_target.Ready();
  cancelled_target.Ready();
  expired_target.Ready();
  late_target.Ready();
  CheckpointAPI api;
  api.RequireCustomStorage();
  CUdevice device;
  CUcontext context;
  Check(cuDeviceGet(&device, 0), "get checkpoint device");
  Check(cuDevicePrimaryCtxRetain(&context, device), "retain checkpoint context");
  Check(cuCtxSetCurrent(context), "select checkpoint context");
  // Cancelling before preparation must leave the live checkpoint target usable.
  {
    Operation cancelled(api, target.pid, OpenPidfd(target.pid));
    cancelled.Lock();
    cancelled.Abort();
    CUprocessState state;
    Check(cuCheckpointProcessGetState(target.pid, &state), "get cancelled target state");
    EXPECT_EQ(state, CU_PROCESS_STATE_RUNNING);
  }
  for (int iteration = 0; iteration < 2; ++iteration) {
    Operation checkpoint(api, target.pid, OpenPidfd(target.pid));
    checkpoint.Lock();
    const auto* saved = checkpoint.PrepareCheckpoint();
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
    Operation restore(api, target.pid, OpenPidfd(target.pid));
    const auto* restored = restore.PrepareRestore({});
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
  // Use the same workloads to exercise the broker with real CUDA and NIXL.
  namespace fs = std::filesystem;
  using namespace snapshot::pagebroker;
  const auto root = fs::temp_directory_path() / ("pagebroker-gpu-" + std::to_string(getpid()));
  fs::remove_all(root);
  fs::create_directories(root / "storage");
  CUuuid uuid;
  Check(cuDeviceGetUuid(&uuid, device), "get workload GPU UUID");
  std::array<unsigned char, 16> uuid_bytes;
  std::copy(std::begin(uuid.bytes), std::end(uuid.bytes), uuid_bytes.begin());
  const auto visible_device = gpu::storage::FormatGPUUUID(uuid_bytes);
  constexpr size_t kTestBufferCount = 2;
  constexpr size_t kTestChunkBytes = 1024 * 1024;
  const gpu::EngineOptions options{kTestBufferCount, kTestChunkBytes, 0};
  gpu::GpuEnginePtr engine = std::make_shared<gpu::GpuEngine>(options);
  ASSERT_TRUE(engine->Available());
  {
    Broker broker(root / "staging", root / "storage", engine);
    test::RequestBuilder requests;
    auto run_gpu = [&](const Request& request, CancellationPtr cancellation = std::make_shared<Cancellation>()) {
      const auto& targets = request.has_checkpoint_gpu() ? request.checkpoint_gpu().targets() : request.restore_gpu().targets();
      std::vector<FileDescriptor> descriptors;
      for (const auto& participant : targets) {
        descriptors.push_back(OpenPidfd(static_cast<int>(participant.target_pid())));
      }
      return broker.HandleGpuRequest(request, std::move(cancellation), std::move(descriptors));
    };

    auto prepare_restore = [&](Target& workload, const std::string& name) {
      const auto source = root / "storage" / name;
      auto prepare = requests.RequestFor("save-" + name);
      test::Configure(prepare.mutable_prepare_direct_checkpoint()->mutable_destination(),
                      prepare.mutable_prepare_direct_checkpoint()->mutable_io_engine(), source);
      auto response = broker.HandleRequest(prepare);
      if (!response.has_direct_checkpoint_directory()) {
        throw std::runtime_error(response.DebugString());
      }
      std::ofstream(fs::path(response.direct_checkpoint_directory().image_directory()) / "image") << "CPU image";
      auto save = requests.RequestFor("save-" + name);
      auto* checkpoint = save.mutable_checkpoint_gpu();
      checkpoint->mutable_context()->add_captured_pids(workload.pid);
      checkpoint->mutable_context()->add_visible_devices(visible_device);
      auto* participant = checkpoint->add_targets();
      participant->set_captured_pid(workload.pid);
      participant->set_target_pid(workload.pid);
      response = run_gpu(save);
      if (!response.has_gpu_checkpoint_complete()) {
        throw std::runtime_error(response.DebugString());
      }
      prepare.mutable_commit();
      response = broker.HandleRequest(prepare);
      if (!response.has_commit_complete()) {
        throw std::runtime_error(response.DebugString());
      }
      auto stage = requests.RequestFor(name);
      test::Configure(stage.mutable_staged_restore()->mutable_source(),
                      stage.mutable_staged_restore()->mutable_io_engine(), source);
      response = broker.HandleRequest(stage);
      if (!response.has_staged_restore_directory()) {
        throw std::runtime_error(response.DebugString());
      }
      auto restore = requests.RequestFor(name);
      *restore.mutable_restore_gpu()->mutable_context() = checkpoint->context();
      *restore.mutable_restore_gpu()->mutable_targets() = checkpoint->targets();
      return RestoreCase{std::move(restore), response.staged_restore_directory().image_directory(), source};
    };

    enum class Interruption { Abort, Expiry };
    auto interrupt_restore = [&](Target& workload, const std::string& name, Interruption interruption) {
      SCOPED_TRACE(name);
      const auto restore = prepare_restore(workload, name);
      auto process = OpenPidfd(workload.pid);
      auto cancellation = std::make_shared<Cancellation>();
      auto finish = requests.RequestFor(name);
      finish.mutable_commit();
      // Release the pause before future destructors join, including on assertion failure.
      std::future<Response> work;
      std::future<Response> abort;
      ScopedPause paused(copy_pause);
      work = std::async(std::launch::async, [&] { return run_gpu(restore.request, cancellation); });
      ASSERT_TRUE(copy_pause.Wait()) << "restore did not submit a CUDA copy";
      ASSERT_EQ(copy_pause.result_, CUDA_SUCCESS);
      // A real NIXL read completed before HtoD submission. The stream gate
      // now holds that real copy pending, not merely a worker before Transfer.
      SelectContext(copy_pause.stream_);
      ASSERT_EQ(cuStreamQuery(copy_pause.stream_), CUDA_ERROR_NOT_READY);
      ASSERT_EQ(work.wait_for(std::chrono::seconds{0}), std::future_status::timeout);
      if (interruption == Interruption::Abort) {
        auto request = requests.RequestFor(name);
        request.mutable_abort();
        abort = std::async(std::launch::async, [&, request] { return broker.HandleRequest(request); });
        ASSERT_EQ(abort.wait_for(kAbortResponseWait), std::future_status::ready);
        const auto response = abort.get();
        ASSERT_TRUE(response.has_failure()) << response.DebugString();
        EXPECT_EQ(response.failure().message(), "GPU cleanup is still running");
      } else {
        broker.ReapExpiredTransactions(std::chrono::steady_clock::now() + kExpiryAdvance);
      }
      EXPECT_TRUE(cancellation->IsCancelled());
      EXPECT_EQ(work.wait_for(std::chrono::seconds{0}), std::future_status::timeout);
      EXPECT_TRUE(fs::exists(restore.staging / "image"));
      EXPECT_TRUE(fs::exists(restore.source / gpu::kDataDirectory));
      pollfd target_state{process.get(), POLLIN, 0};
      EXPECT_EQ(poll(&target_state, 1, 0), 0) << "target exited before CUDA drained";
      const auto rejected = broker.HandleRequest(finish);
      ASSERT_TRUE(rejected.has_failure()) << rejected.DebugString();
      EXPECT_EQ(rejected.failure().message(), "transaction is aborting");

      copy_pause.Release();
      ASSERT_EQ(work.wait_for(kGpuTestWait), std::future_status::ready);
      const auto failed = work.get();
      ASSERT_TRUE(failed.has_failure()) << failed.DebugString();
      EXPECT_EQ(failed.failure().code(), Failure::INTERNAL_ERROR);
      EXPECT_EQ(poll(&target_state, 1, 0), 1) << "failed restore target is still running";
      EXPECT_NE(target_state.revents & (POLLIN | POLLHUP), 0);
      if (interruption == Interruption::Abort) {
        finish.mutable_abort();
        const auto drained = broker.HandleRequest(finish);
        EXPECT_TRUE(drained.has_abort_complete()) << drained.DebugString();
      } else {
        broker.ReapExpiredTransactions(std::chrono::steady_clock::now() + kExpiryAdvance);
      }
      EXPECT_FALSE(fs::exists(restore.staging));
      EXPECT_TRUE(fs::exists(restore.source / gpu::kDataDirectory));
    };
    ASSERT_NO_FATAL_FAILURE(interrupt_restore(cancelled_target, "cancel-active", Interruption::Abort));
    ASSERT_NO_FATAL_FAILURE(interrupt_restore(expired_target, "expire-active", Interruption::Expiry));

    {
      const auto restore = prepare_restore(late_target, "late-success");
      std::future<Response> work;
      ScopedPause paused(unlock_pause);
      work = std::async(std::launch::async, [&] { return run_gpu(restore.request); });
      ASSERT_TRUE(unlock_pause.Wait()) << "restore did not unlock its target";
      ASSERT_EQ(unlock_pause.result_, CUDA_SUCCESS);
      broker.ReapExpiredTransactions(std::chrono::steady_clock::now() + kExpiryAdvance);
      EXPECT_TRUE(fs::exists(restore.staging / "image"));
      unlock_pause.Release();
      ASSERT_EQ(work.wait_for(kGpuTestWait), std::future_status::ready);
      const auto restored = work.get();
      ASSERT_TRUE(restored.has_gpu_restore_complete()) << restored.DebugString();
      auto commit = requests.RequestFor("late-success");
      commit.mutable_commit();
      const auto rejected = broker.HandleRequest(commit);
      ASSERT_TRUE(rejected.has_failure()) << rejected.DebugString();
      EXPECT_EQ(rejected.failure().message(), "transaction is aborting");
      EXPECT_TRUE(fs::exists(restore.staging / "image"));
      broker.ReapExpiredTransactions(std::chrono::steady_clock::now() + kExpiryAdvance);
      EXPECT_FALSE(fs::exists(restore.staging));
      EXPECT_TRUE(fs::exists(restore.source / gpu::kDataDirectory));
      EXPECT_EQ(late_target.Verify(), 0);
    }

    // A complete round trip after both failures proves the same engine and
    // transfer buffers still work for other participants.
    const auto destination = root / "storage" / "batch";
    auto prepare = requests.RequestFor("save-batch");
    auto* storage = prepare.mutable_prepare_direct_checkpoint();
    test::Configure(storage->mutable_destination(), storage->mutable_io_engine(), destination);
    const auto prepared = broker.HandleRequest(prepare);
    ASSERT_TRUE(prepared.has_direct_checkpoint_directory()) << prepared.DebugString();
    std::ofstream(fs::path(prepared.direct_checkpoint_directory().image_directory()) / "image") << "CPU image";

    auto save = requests.RequestFor("save-batch");
    auto* checkpoint = save.mutable_checkpoint_gpu();
    checkpoint->mutable_context()->add_visible_devices(visible_device);
    for (const Target* workload : {&target, &next_target}) {
      checkpoint->mutable_context()->add_captured_pids(workload->pid);
      auto* participant = checkpoint->add_targets();
      participant->set_captured_pid(workload->pid);
      participant->set_target_pid(workload->pid);
    }
    auto invalid = save;
    invalid.mutable_checkpoint_gpu()->mutable_context()->set_visible_devices(0, "invalid-uuid");
    const auto rejected = run_gpu(invalid);
    ASSERT_TRUE(rejected.has_failure()) << rejected.DebugString();
    EXPECT_EQ(rejected.failure().code(), Failure::INVALID_REQUEST);

    const auto saved = run_gpu(save);
    ASSERT_TRUE(saved.has_gpu_checkpoint_complete()) << saved.DebugString();
    ASSERT_EQ(saved.gpu_checkpoint_complete().participants_size(), 2);

    // Reordering a participant set must reuse the saved result, not execute CUDA again.
    save.set_request_id(requests.RequestFor("save-batch").request_id());
    checkpoint->mutable_targets()->SwapElements(0, 1);
    checkpoint->mutable_context()->mutable_captured_pids()->SwapElements(0, 1);
    const auto repeated = run_gpu(save);
    ASSERT_TRUE(repeated.has_gpu_checkpoint_complete()) << repeated.DebugString();
    EXPECT_EQ(saved.gpu_checkpoint_complete().SerializeAsString(), repeated.gpu_checkpoint_complete().SerializeAsString());
    EXPECT_EQ(repeated.request_id(), save.request_id());
    auto changed = save;
    changed.mutable_checkpoint_gpu()->mutable_targets()->SwapElements(0, 1);
    auto* changed_targets = changed.mutable_checkpoint_gpu()->mutable_targets();
    const auto first_pid = changed_targets->Get(0).target_pid();
    changed_targets->Mutable(0)->set_target_pid(changed_targets->Get(1).target_pid());
    changed_targets->Mutable(1)->set_target_pid(first_pid);
    const auto conflict = run_gpu(changed);
    ASSERT_TRUE(conflict.has_failure()) << conflict.DebugString();
    EXPECT_EQ(conflict.failure().code(), Failure::TRANSACTION_CONFLICT);
    auto commit = requests.RequestFor("save-batch");
    commit.mutable_commit();
    const auto committed = broker.HandleRequest(commit);
    ASSERT_TRUE(committed.has_commit_complete()) << committed.DebugString();

    auto stage = requests.RequestFor("restore-batch");
    auto* restore_storage = stage.mutable_staged_restore();
    test::Configure(restore_storage->mutable_source(), restore_storage->mutable_io_engine(), destination);
    const auto staged = broker.HandleRequest(stage);
    ASSERT_TRUE(staged.has_staged_restore_directory()) << staged.DebugString();
    const fs::path directory(staged.staged_restore_directory().image_directory());
    EXPECT_TRUE(fs::exists(directory / "image"));
    EXPECT_FALSE(fs::exists(directory / gpu::kDataDirectory));
    auto restore = requests.RequestFor("restore-batch");
    *restore.mutable_restore_gpu()->mutable_context() = checkpoint->context();
    *restore.mutable_restore_gpu()->mutable_targets() = checkpoint->targets();
    const auto restored = run_gpu(restore);
    ASSERT_TRUE(restored.has_gpu_restore_complete()) << restored.DebugString();
    ASSERT_EQ(restored.gpu_restore_complete().participants_size(), 2);
    auto finish = requests.RequestFor("restore-batch");
    finish.mutable_commit();
    const auto completed = broker.HandleRequest(finish);
    EXPECT_TRUE(completed.has_commit_complete()) << completed.DebugString();
    EXPECT_FALSE(fs::exists(directory));
    EXPECT_TRUE(fs::exists(destination / gpu::kDataDirectory));
  }
  engine.reset();
  EXPECT_EQ(next_target.Verify(), 0);
  fs::remove_all(root);
  Operation exited(api, target.pid, OpenPidfd(target.pid));
  EXPECT_EQ(target.Verify(), 0);
  try {
    exited.CheckTarget();
    FAIL() << "reaped CUDA target was reported as running";
  } catch (const std::runtime_error& error) {
    EXPECT_STREQ(error.what(), "CUDA target exited");
  }
  Check(cuDevicePrimaryCtxRelease(device), "release checkpoint context");
}
}  // namespace

extern "C" CUresult CUDAAPI __real_cuMemcpyHtoDAsync_v2(CUdeviceptr, const void*, size_t, CUstream);
extern "C" CUresult CUDAAPI __real_cuCheckpointProcessUnlock(int, CUcheckpointUnlockArgs*);

extern "C" CUresult CUDAAPI
__wrap_cuMemcpyHtoDAsync_v2(CUdeviceptr device, const void* host, size_t bytes, CUstream stream)
{
  if (!copy_pause.Claim()) {
    return __real_cuMemcpyHtoDAsync_v2(device, host, bytes, stream);
  }
  // The callback only waits on a CPU latch. The test releases it before
  // waiting for CUDA completion. No CUDA API runs inside the callback.
  const auto gate = cuLaunchHostFunc(stream, [](void*) { copy_pause.Hold(); }, nullptr);
  const auto result = __real_cuMemcpyHtoDAsync_v2(device, host, bytes, stream);
  copy_pause.Arrive(result == CUDA_SUCCESS ? gate : result, stream);
  return result;
}

extern "C" CUresult CUDAAPI
__wrap_cuCheckpointProcessUnlock(int pid, CUcheckpointUnlockArgs* arguments)
{
  const auto result = __real_cuCheckpointProcessUnlock(pid, arguments);
  if (unlock_pause.Claim()) {
    unlock_pause.Arrive(result);
    unlock_pause.Hold();
  }
  return result;
}

TEST(GpuTargetDescriptor, ReportsMissingFdinfoSeparatelyFromPidMismatch)
{
  using snapshot::pagebroker::gpu::driver::ValidateTargetDescriptor;
  const int other_pid = getpid() + 1;
  auto expect_message = [&](int descriptor, const char* expected) {
    try {
      ValidateTargetDescriptor(other_pid, descriptor);
      FAIL() << "invalid descriptor accepted";
    } catch (const std::exception& error) {
      EXPECT_NE(std::string(error.what()).find(expected), std::string::npos);
    }
  };
  expect_message(std::numeric_limits<int>::max(), "read GPU target fdinfo");
  FileDescriptor file(open("/dev/null", O_RDONLY | O_CLOEXEC));
  ASSERT_GE(file.get(), 0);
  expect_message(file.get(), "fdinfo has no PID entry");
  auto self = OpenPidfd(getpid());
  expect_message(self.get(), "does not match host PID");
}

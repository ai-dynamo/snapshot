// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0
#include "allocation_transport.hpp"
#include "gpu_engine.pb.h"
#include "cuda_posix_transfer.hpp"
#include "../cmd/cuda-checkpoint-helper/storage_manifest.hpp"

#include <fcntl.h>
#include <poll.h>
#include <signal.h>
#include <sys/prctl.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <unistd.h>
#include <algorithm>
#include <chrono>
#include <cstdio>
#include <future>
#include <map>
#include <mutex>
#include <stdexcept>
#include <thread>

namespace transfer = cuda_checkpoint_transfer;
namespace storage = cuda_checkpoint_storage;
using namespace snapshot::pagebroker;
using Clock = std::chrono::steady_clock;

namespace {
void Check(CUresult result, const char* operation) {
  if (result == CUDA_SUCCESS) return;
  const char* name = nullptr;
  cuGetErrorName(result, &name);
  throw std::runtime_error(std::string(operation) + ": " + (name ? name : "unknown CUDA error"));
}
void Require(bool ok, const char* message) {
  if (!ok) throw std::runtime_error(message);
}
double Seconds(Clock::time_point start) {
  return std::chrono::duration<double>(Clock::now() - start).count();
}
long long Nanoseconds(Clock::time_point time) {
  return std::chrono::duration_cast<std::chrono::nanoseconds>(time.time_since_epoch()).count();
}
template <typename... Args>
std::string Report(const char* format, Args... args) {
  char buffer[2048];
  const int length = std::snprintf(buffer, sizeof(buffer), format, args...);
  Require(length >= 0 && static_cast<size_t>(length) < sizeof(buffer), "native report overflow");
  return std::string(buffer, length);
}

struct Device {
  CUdevice device;
  CUcontext context;
  std::string uuid;
  std::mutex mutex;
  transfer::TransferBuffers buffers{{32, 32ULL * 1024 * 1024}};
};

// Immutable device ownership, initialized before the CPU broker starts serving.
// Each ring has its own NIXL agent, pinned memory and context-local CUDA events.
// Sessions on different GPUs transfer concurrently; those sharing a GPU lease
// the same ring. Native operation handles and streams remain session-local.
class Engine {
 public:
  Engine() {
    Require(!std::getenv("CUDA_CHECKPOINT_JOB_FILE"), "GPU engine requires no CUDA_CHECKPOINT_JOB_FILE");
    const auto start = Clock::now();
    Check(cuInit(0), "cuInit");
    void* symbol = nullptr;
    CUdriverProcAddressQueryResult query;
    Check(cuGetProcAddress("cuCheckpointOperationComplete", &symbol, 13040,
                          CU_GET_PROC_ADDRESS_DEFAULT, &query), "resolve COMPLETE");
    Require(symbol && query == CU_GET_PROC_ADDRESS_SUCCESS, "driver does not expose CustomStorage COMPLETE");
    complete = reinterpret_cast<decltype(complete)>(symbol);
    int count = 0;
    Check(cuDeviceGetCount(&count), "cuDeviceGetCount");
    for (int index = 0; index < count; ++index) {
      auto owner = std::make_unique<Device>();
      Check(cuDeviceGet(&owner->device, index), "cuDeviceGet");
      CUuuid uuid;
      Check(cuDeviceGetUuid(&uuid, owner->device), "cuDeviceGetUuid");
      std::array<unsigned char, 16> bytes{};
      std::copy(std::begin(uuid.bytes), std::end(uuid.bytes), bytes.begin());
      owner->uuid = storage::FormatGPUUUID(bytes);
      Check(cuDevicePrimaryCtxRetain(&owner->context, owner->device), "retain engine context");
      std::string error;
      if (!owner->buffers.Initialize(owner->context, &error)) throw std::runtime_error(error);
      devices.emplace(owner->context, std::move(owner));
    }
    Require(!devices.empty(), "GPU engine has no devices");
    ready = Report("{\"event\":\"ready\",\"initialization_seconds\":%.6f,\"retained_contexts\":%zu,"
                   "\"pinned_bytes_per_device\":1073741824,\"buffer_count\":32,\"chunk_bytes\":33554432}", Seconds(start), devices.size());
  }
  std::map<CUcontext, std::unique_ptr<Device>> devices;
  decltype(&cuCheckpointOperationComplete) complete = nullptr;
  std::string ready;
};

class Operation {
 public:
  Operation(Engine& engine, const internal::NativeBinding& admission, FileDescriptor directory)
      : engine_(engine), directory_fd_(std::move(directory)),
        directory_("/proc/self/fd/" + std::to_string(directory_fd_.get())),
        pid_(admission.host_pid()), save_(admission.binding().direction() == v1::BindAllocationSession::SAVE),
        pidfd_(static_cast<int>(syscall(SYS_pidfd_open, pid_, 0))) {
    Require(pid_ > 0 && pid_ != getpid() && pidfd_.get() >= 0, "pin native target");
    struct stat info{};
    Require(!fstat(directory_fd_.get(), &info) && S_ISDIR(info.st_mode) && !(info.st_mode & 0022),
            "native storage must be a private directory");
    for (const auto& uuid : admission.binding().visible_devices()) {
      Require(std::any_of(engine_.devices.begin(), engine_.devices.end(),
                         [&](const auto& device) { return device.second->uuid == uuid; }),
              "target GPU is not available in the persistent engine");
    }
    const std::string& mapping = admission.binding().device_map();
    size_t offset = 0;
    while (offset < mapping.size()) {
      const auto end = mapping.find(',', offset);
      const auto pair = mapping.substr(offset, end - offset);
      const auto equal = pair.find('=');
      std::array<unsigned char, 16> source{}, destination{};
      Require(equal != std::string::npos && storage::ParseGPUUUID(pair.substr(0, equal), &source) &&
              storage::ParseGPUUUID(pair.substr(equal + 1), &destination), "invalid native GPU map");
      CUcheckpointGpuPair gpu{};
      std::copy(source.begin(), source.end(), gpu.oldUuid.bytes);
      std::copy(destination.begin(), destination.end(), gpu.newUuid.bytes);
      gpu_pairs_.push_back(gpu);
      device_pairs_.push_back({pair.substr(0, equal), pair.substr(equal + 1)});
      if (end == std::string::npos) break;
      offset = end + 1;
    }
    Check(cuCtxSetCurrent(engine_.devices.begin()->first), "set engine session context");
  }

  std::string Execute(const v1::NativeSessionRequest& request) {
    using Op = v1::NativeSessionRequest;
    const auto next = phase_ == Op::UNSPECIFIED ? (save_ ? Op::LOCK : Op::PREPARE) :
                      phase_ == Op::LOCK ? Op::PREPARE :
                      phase_ == Op::PREPARE ? Op::TRANSFER : Op::COMPLETE;
    Require(phase_ != Op::COMPLETE && request.operation() == next, "invalid native phase order");
    pollfd target{pidfd_.get(), POLLIN, 0};
    Require(poll(&target, 1, 0) == 0, "native target exited");
    std::string result;
    if (next == Op::LOCK) {
      CUcheckpointLockArgs args{};
      args.timeoutMs = 10000;
      touched_ = true;
      Check(cuCheckpointProcessLock(pid_, &args), "native lock");
      result = "{\"event\":\"locked\"}";
    } else if (next == Op::PREPARE) result = Prepare();
    else if (next == Op::TRANSFER) result = Transfer();
    else result = Complete();
    phase_ = next;
    return result;
  }

  void Drain() {
    if (phase_ == v1::NativeSessionRequest::COMPLETE) return;
    // No public abort exists. Once native preparation has begun, the target is
    // unusable after cancellation. Kill through its pidfd, then COMPLETE the
    // abandoned operation to release caller-side streams/imported mappings.
    // Execute is synchronous: all NIXL I/O and DMA finish before we get here.
    if (touched_) {
      if (syscall(SYS_pidfd_send_signal, pidfd_.get(), SIGKILL, nullptr, 0) && errno != ESRCH)
        throw std::runtime_error("terminate cancelled native target");
      pollfd target{pidfd_.get(), POLLIN, 0};
      int status;
      do { status = poll(&target, 1, -1); } while (status < 0 && errno == EINTR);
      Require(status > 0, "wait for cancelled target");
    }
    if (view_) {
      auto handle = view_->handle;
      view_ = nullptr;
      // On the qualified CustomStorage driver, COMPLETE destroys the operation
      // even when the now-dead target rejects completion. Contexts stay alive.
      const auto result = engine_.complete(handle);
      std::fprintf(stderr, "Cancelled native target=%d COMPLETE result=%d; mappings drained\n", pid_, result);
    }
  }

 private:
  std::string Prepare() {
    start_ = Clock::now();
    std::string error;
    if (save_) {
      if (!storage::RemoveManifest(directory_, &error)) throw std::runtime_error(error);
    } else if (!storage::ReadManifest(directory_, &manifest_, &error) ||
               !storage::ValidateExtentFiles(directory_, manifest_, &error)) throw std::runtime_error(error);
    const auto begin = Clock::now();
    touched_ = true;
    if (save_) {
      CUcheckpointCheckpointArgs args{};
      args.customStorageInfo_out = &view_;
      Check(cuCheckpointProcessCheckpoint(pid_, &args), "native checkpoint prepare");
    } else {
      CUcheckpointRestoreArgs args{};
      args.customStorageInfo_out = &view_;
      args.gpuPairs = gpu_pairs_.data();
      args.gpuPairsCount = gpu_pairs_.size();
      Check(cuCheckpointProcessRestore(pid_, &args), "native restore prepare");
    }
    const auto end = Clock::now();
    prepare_seconds_ = std::chrono::duration<double>(end - begin).count();
    Require(view_ && view_->handle && view_->deviceCount <= engine_.devices.size() &&
            (!view_->deviceCount || view_->perDeviceData), "invalid CustomStorage view");
    std::vector<storage::DeviceExtent> extents;
    for (unsigned index = 0; index < view_->deviceCount; ++index) {
      CUcontext context;
      Check(cuStreamGetCtx(view_->perDeviceData[index].stream, &context), "stream context");
      auto& device = *engine_.devices.at(context);
      owners_.push_back(&device);
      extents.push_back({device.uuid, view_->perDeviceData[index].size});
    }
    if (save_ && !storage::BuildCheckpointManifest(extents, &manifest_, &error)) throw std::runtime_error(error);
    if (!storage::BuildTransferJobs(manifest_, extents, device_pairs_, &jobs_, &error)) throw std::runtime_error(error);
    return Report("{\"event\":\"prepared\",\"native_prepare_seconds\":%.6f,\"prepare_start_ns\":%lld,"
                  "\"prepare_end_ns\":%lld}", prepare_seconds_, Nanoseconds(begin), Nanoseconds(end));
  }

  std::string Transfer() {
    const auto begin = Clock::now();
    for (const auto& job : jobs_) {
      const auto& data = view_->perDeviceData[job.device_index];
      if (!data.size) continue;
      const auto path = directory_ / manifest_[job.extent_index].filename;
      FileDescriptor file(open(path.c_str(), O_CLOEXEC | O_NOFOLLOW |
                                (save_ ? O_CREAT | O_TRUNC | O_RDWR : O_RDONLY), 0600));
      Require(file.get() >= 0 && (!save_ || !ftruncate(file.get(), data.size)), "open or size native extent");
      transfer::StorageLayout layout{{{path, data.size, file.get()}}, {{0, data.size, 0, 0}}};
      transfer::TransferMetrics metrics;
      auto& device = *owners_[job.device_index];
      std::lock_guard lease(device.mutex);
      std::string error;
      if (!device.buffers.Transfer(data.devPtr, data.size, data.stream, device.context, layout,
                                   save_ ? transfer::TransferOperation::kCheckpoint : transfer::TransferOperation::kRestore,
                                   nullptr, &metrics, &error)) throw std::runtime_error(error);
      bytes_ += metrics.bytes;
      setup_seconds_ += metrics.setup_seconds;
      storage_seconds_ += metrics.storage_io_seconds;
      cuda_wait_seconds_ += metrics.cuda_wait_seconds;
    }
    const auto end = Clock::now();
    transfer_seconds_ = std::chrono::duration<double>(end - begin).count();
    return Report("{\"event\":\"transferred\",\"bytes\":%zu,\"transfer_seconds\":%.6f,"
                  "\"transfer_start_ns\":%lld,\"transfer_end_ns\":%lld}", bytes_, transfer_seconds_,
                  Nanoseconds(begin), Nanoseconds(end));
  }

  std::string Complete() {
    const auto begin = Clock::now();
    auto handle = view_->handle;
    view_ = nullptr;
    Check(engine_.complete(handle), "native COMPLETE");
    const double completion = Seconds(begin);
    std::string error;
    double unlock = 0;
    if (save_) {
      if (!storage::WriteManifest(directory_, manifest_, &error)) throw std::runtime_error(error);
    } else {
      const auto unlock_start = Clock::now();
      Check(cuCheckpointProcessUnlock(pid_, nullptr), "native unlock");
      unlock = Seconds(unlock_start);
    }
    return Report("{\"event\":\"complete\",\"bytes\":%zu,\"native_prepare_seconds\":%.6f,"
                  "\"transfer_seconds\":%.6f,\"transfer_setup_seconds\":%.6f,"
                  "\"storage_request_service_seconds\":%.6f,\"cuda_wait_seconds\":%.6f,"
                  "\"complete_seconds\":%.6f,\"unlock_seconds\":%.6f,\"total_seconds\":%.6f}",
                  bytes_, prepare_seconds_, transfer_seconds_, setup_seconds_, storage_seconds_, cuda_wait_seconds_,
                  completion, unlock, Seconds(start_));
  }

  Engine& engine_;
  FileDescriptor directory_fd_;
  std::filesystem::path directory_;
  int pid_;
  bool save_;
  bool touched_ = false;
  FileDescriptor pidfd_;
  v1::NativeSessionRequest::Operation phase_ = v1::NativeSessionRequest::UNSPECIFIED;
  CUcheckpointCustomStorageInfo* view_ = nullptr;
  std::vector<CUcheckpointGpuPair> gpu_pairs_;
  std::vector<storage::DevicePair> device_pairs_;
  std::vector<storage::ManifestExtent> manifest_;
  std::vector<storage::TransferJob> jobs_;
  std::vector<Device*> owners_;
  Clock::time_point start_;
  size_t bytes_ = 0;
  double prepare_seconds_ = 0, transfer_seconds_ = 0, setup_seconds_ = 0, storage_seconds_ = 0, cuda_wait_seconds_ = 0;
};

void ServeSession(Engine& engine, internal::NativeBinding binding, FileDescriptor connection,
                  FileDescriptor directory) {
  std::unique_ptr<Operation> operation;
  v1::NativeSessionReply reply;
  try {
    operation = std::make_unique<Operation>(engine, binding, std::move(directory));
    reply.set_report("{\"event\":\"ready\",\"persistent_engine\":true}");
  } catch (const std::exception& error) {
    reply.mutable_failure()->set_code(v1::Failure::INTERNAL_ERROR);
    reply.mutable_failure()->set_message(error.what());
  }
  try {
    SendFrame(connection.get(), reply);
    internal::NativeCommand command;
    std::vector<FileDescriptor> descriptors;
    while (ReceiveFrame(connection.get(), command, descriptors)) {
      Require(descriptors.empty(), "GPU engine command cannot carry descriptors");
      if (command.has_drain()) break;
      reply.Clear();
      try {
        Require(operation && command.has_execute(), "expected native operation");
        reply.set_report(operation->Execute(command.execute()));
      } catch (const std::exception& error) {
        reply.mutable_failure()->set_code(v1::Failure::INTERNAL_ERROR);
        reply.mutable_failure()->set_message(error.what());
      }
      SendFrame(connection.get(), reply);
    }
  } catch (const std::exception& error) {
    std::fprintf(stderr, "Native session target=%u disconnected: %s\n", binding.host_pid(), error.what());
  }
  // A failed drain is engine-fatal; do not acknowledge or release admission
  // while imported memory could still be in use. The broker reaps the engine.
  try {
    if (operation) operation->Drain();
  } catch (const std::exception& error) {
    std::fprintf(stderr, "GPU engine drain failed: %s\n", error.what());
    std::_Exit(1);
  }
  reply.Clear();
  reply.set_report("drained");
  try { SendFrame(connection.get(), reply); } catch (const std::exception&) {}
}
}  // namespace

int main() {
  try {
    // The CPU broker is the sole owner. An orphan engine must not retain CUDA
    // contexts and checkpoint descriptors after the broker has exited.
    const auto parent = getppid();
    Require(!prctl(PR_SET_PDEATHSIG, SIGKILL) && getppid() == parent, "GPU engine parent exited");
    Engine engine;
    v1::NativeSessionReply ready;
    ready.set_report(engine.ready);
    SendFrame(3, ready);
    std::vector<std::future<void>> sessions;
    internal::NativeBinding binding;
    std::vector<FileDescriptor> descriptors;
    while (ReceiveFrame(3, binding, descriptors)) {
      Require(descriptors.size() == 2, "GPU admission requires session and directory descriptors");
      std::erase_if(sessions, [](auto& session) {
        if (session.wait_for(std::chrono::seconds(0)) != std::future_status::ready) return false;
        session.get();
        return true;
      });
      sessions.push_back(std::async(std::launch::async, ServeSession, std::ref(engine), binding,
                                    std::move(descriptors[0]), std::move(descriptors[1])));
      descriptors.clear();
    }
    // Process exit is the engine's context-lifetime boundary. No per-session
    // teardown releases primary contexts used by other restored processes.
    std::_Exit(0);
  } catch (const std::exception& error) {
    std::fprintf(stderr, "GPU engine failed: %s\n", error.what());
    std::_Exit(1);
  }
}

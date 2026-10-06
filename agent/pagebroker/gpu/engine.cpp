// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0
#include "engine.hpp"

#include <nvtx3/nvtx3.hpp>
#include <fcntl.h>
#include <sys/stat.h>
#include <unistd.h>
#include <algorithm>
#include <array>
#include <climits>
#include <cstdlib>
#include <exception>
#include <filesystem>
#include <cstdio>
#include <limits>
#include <map>
#include <mutex>
#include <set>
#include <thread>

#include "checkpoint.hpp"
#include "file_descriptor.hpp"
#include "storage_manifest.hpp"
#include "transfer.hpp"

namespace snapshot::pagebroker::gpu {
namespace detail {
namespace transfer = snapshot::pagebroker::cuda;
void Require(bool condition, const char* message) {
  if (!condition) throw std::runtime_error(message);
}
void Check(CUresult status, const char* operation) {
  if (status == CUDA_SUCCESS) return;
  const char* name = nullptr;
  cuGetErrorName(status, &name);
  throw std::runtime_error(std::string(operation) + ": " + (name ? name : "unknown CUDA error"));
}
void LogCurrentException(const char* operation) noexcept {
  try {
    throw;
  } catch (const std::exception& error) {
    std::fprintf(stderr, "%s: %s\n", operation, error.what());
  } catch (...) {
    std::fprintf(stderr, "%s: unknown exception\n", operation);
  }
}
FileDescriptor Duplicate(int fd) {
  FileDescriptor result(fcntl(fd, F_DUPFD_CLOEXEC, 0));
  Require(result.get() >= 0, "duplicate GPU artifact descriptor");
  return result;
}
void CheckCancellation(const Cancellation& cancellation) {
  Require(!cancellation.IsCancelled(), "GPU operation cancelled");
}
struct Device {
  explicit Device(transfer::TransferOptions options) : buffers(options) {}
  CUdevice device = 0;
  CUcontext context = nullptr;
  std::string uuid;
  std::mutex mutex;
  transfer::TransferBuffers buffers;
};
struct Resources {
  explicit Resources(const EngineOptions& options) {
    Require(!std::getenv("CUDA_CHECKPOINT_JOB_FILE"), "CUDA_CHECKPOINT_JOB_FILE must be unset");
    if (!checkpoint.SupportsCustomStorage()) return;
    int count = 0;
    Check(cuDeviceGetCount(&count), "cuDeviceGetCount");
    if (!count) return;
    const transfer::TransferOptions settings{options.buffer_count, options.chunk_bytes, true};
    transfer::TransferMemoryBytes(settings, count, options.max_pinned_bytes);
    for (int index = 0; index < count; ++index) {
      auto device = std::make_unique<Device>(settings);
      Check(cuDeviceGet(&device->device, index), "cuDeviceGet");
      CUuuid uuid;
      Check(cuDeviceGetUuid(&uuid, device->device), "cuDeviceGetUuid");
      std::array<unsigned char, 16> bytes{};
      std::copy(std::begin(uuid.bytes), std::end(uuid.bytes), bytes.begin());
      device->uuid = storage::FormatGPUUUID(bytes);
      Check(cuDevicePrimaryCtxRetain(&device->context, device->device), "retain PageBroker context");
      devices.emplace(device->device, std::move(device));
    }
    size_t total_bytes = 0;
    for (const auto& [id, device] : devices) {
      (void)id;
      const size_t bytes = transfer::TransferBuffers::AllocationBytes(device->context, settings);
      Require(bytes <= std::numeric_limits<size_t>::max() - total_bytes, "total pinned allocation overflow");
      total_bytes += bytes;
      Require(!options.max_pinned_bytes || total_bytes <= options.max_pinned_bytes,
              "total pinned allocation exceeds max-pinned-bytes");
    }
    for (const auto& [id, device] : devices) {
      (void)id;
      device->buffers.Initialize(device->context);
    }
    available = true;
  }
  Device& DeviceForContext(CUcontext context) {
    for (auto& [id, device] : devices) {
      (void)id;
      if (device->context == context) return *device;
    }
    throw std::runtime_error("CustomStorage returned an unknown CUDA context");
  }
  driver::CheckpointAPI checkpoint;
  std::map<CUdevice, std::unique_ptr<Device>> devices;
  bool available = false;
  std::atomic<bool> fatal{false};
  std::mutex targets_mutex;
  std::set<int> active_targets;
};
class TargetLease {
 public:
  TargetLease(Resources& engine, std::vector<int> targets) : engine_(engine), targets_(std::move(targets)) {
    std::lock_guard lock(engine_.targets_mutex);
    if (engine_.fatal.load()) throw FatalError("GPU engine is unavailable after a fatal error");
    for (const auto target : targets_)
      if (engine_.active_targets.contains(target))
        throw TargetConflict("GPU target belongs to another active transaction");
    size_t claimed = 0;
    try {
      for (const auto target : targets_) {
        engine_.active_targets.insert(target);
        ++claimed;
      }
    } catch (...) {
      for (size_t index = 0; index < claimed; ++index) engine_.active_targets.erase(targets_[index]);
      throw;
    }
  }
  ~TargetLease() {
    std::lock_guard lock(engine_.targets_mutex);
    for (const auto target : targets_) engine_.active_targets.erase(target);
  }
 private:
  Resources& engine_;
  std::vector<int> targets_;
};
struct PreparedParticipant {
  uint32_t captured_pid;
  FileDescriptor directory;
  std::vector<storage::GpuDataFile> manifest;
};
class Operation {
 public:
  Operation(Resources& engine, PreparedParticipant& prepared,
            uint32_t target_pid, bool save, const std::vector<storage::DevicePair>& mapping,
            std::vector<CUcheckpointGpuPair>& pairs, Cancellation& cancellation, ParticipantResult& result)
      : engine_(engine), prepared_(prepared), save_(save), mapping_(mapping), pairs_(pairs),
        cancellation_(cancellation), result_(result) {
    Require(target_pid > 0 && target_pid <= INT_MAX, "invalid host PID");
    host_pid_ = static_cast<int>(target_pid);
    cuda_ = std::make_unique<driver::Operation>(engine.checkpoint, host_pid_);
    result_.captured_pid = prepared.captured_pid;
  }
  void Lock() {
    const nvtx3::scoped_range range{"PageBroker checkpoint lock"};
    cuda_->CheckTarget();
    cuda_->Lock();
  }
  int HostPid() const { return host_pid_; }
  void Prepare() {
    const nvtx3::scoped_range range{"PageBroker CUDA setup", nvtx3::payload{prepared_.captured_pid}};
    CheckCancellation(cancellation_);
    cuda_->CheckTarget();
    Check(cuCtxSetCurrent(engine_.devices.begin()->second->context), "set GPU operation context");
    view_ = cuda_->Prepare(save_, pairs_);
    Require(view_->deviceCount <= engine_.devices.size(), "too many CustomStorage devices");
    std::vector<storage::GpuDataRegion> extents;
    for (unsigned index = 0; index < view_->deviceCount; ++index) {
      CUcontext context = nullptr;
      Check(cuStreamGetCtx(view_->perDeviceData[index].stream, &context), "stream context");
      auto& device = engine_.DeviceForContext(context);
      owners_.push_back(&device);
      extents.push_back({device.uuid, view_->perDeviceData[index].size});
    }
    std::string error;
    if (save_ && !storage::BuildCheckpointManifest(extents, &prepared_.manifest, &error))
      throw std::runtime_error(error);
    if (!storage::BuildTransferJobs(prepared_.manifest, extents, mapping_, &jobs_, &error))
      throw std::runtime_error(error);
  }
  void Transfer() {
    const nvtx3::scoped_range range{"PageBroker participant transfer", nvtx3::payload{prepared_.captured_pid}};
    for (const auto& job : jobs_) {
      CheckCancellation(cancellation_);
      const auto& data = view_->perDeviceData[job.device_index];
      file_ = FileDescriptor(openat(prepared_.directory.get(), prepared_.manifest[job.extent_index].filename.c_str(),
                                   O_CLOEXEC | O_NOFOLLOW | O_DIRECT | (save_ ? O_CREAT | O_EXCL | O_RDWR : O_RDONLY), 0600));
      Require(file_.get() >= 0 && (!save_ || !ftruncate(file_.get(), data.size)), "open or size GPU extent");
      auto& device = *owners_[job.device_index];
      std::lock_guard lease(device.mutex);
      CheckCancellation(cancellation_);
      device.buffers.Transfer(file_.get(), data.devPtr, data.size, data.stream,
          save_ ? transfer::TransferOperation::kCheckpoint : transfer::TransferOperation::kRestore, {&cancellation_});
      file_ = FileDescriptor(-1);
      result_.bytes += data.size;
    }
  }
  void Complete() {
    const nvtx3::scoped_range range{"PageBroker CUDA completion", nvtx3::payload{prepared_.captured_pid}};
    cuda_->Complete();
  }
  void WriteManifest() {
    std::string error;
    const auto directory = "/proc/self/fd/" + std::to_string(prepared_.directory.get());
    if (!storage::WriteManifest(directory, prepared_.manifest, &error)) throw std::runtime_error(error);
  }
  void Unlock() {
    const nvtx3::scoped_range range{"PageBroker CUDA unlock", nvtx3::payload{prepared_.captured_pid}};
    cuda_->Unlock();
  }
  void Terminate() { cuda_->Terminate(); }
 private:
  Resources& engine_;
  PreparedParticipant& prepared_;
  bool save_;
  const std::vector<storage::DevicePair>& mapping_;
  std::vector<CUcheckpointGpuPair>& pairs_;
  Cancellation& cancellation_;
  ParticipantResult& result_;
  std::unique_ptr<driver::Operation> cuda_;
  int host_pid_ = -1;
  const CUcheckpointCustomStorageInfo* view_ = nullptr;
  std::vector<Device*> owners_;
  std::vector<storage::TransferJob> jobs_;
  // Retained until drain, including failures. Never close an FD underneath NIXL.
  FileDescriptor file_{-1};
};
}  // namespace detail
using namespace detail;

struct Artifact::Impl {
  FileDescriptor root;
  Direction direction;
  std::vector<std::string> visible_devices;
  std::vector<storage::DevicePair> mapping;
  std::vector<CUcheckpointGpuPair> pairs;
  std::vector<PreparedParticipant> participants;
  std::atomic<bool> consumed{false};
  Impl(int directory_fd, Direction direction)
      : root(Duplicate(directory_fd)), direction(direction) {}
};

Artifact::Artifact(int directory_fd, Direction direction, std::vector<uint32_t> captured_pids,
                   std::vector<std::string> visible_devices, std::vector<DeviceMapping> device_map)
    : impl_(std::make_unique<Impl>(directory_fd, direction)) {
  struct stat info{};
  Require(!fstat(impl_->root.get(), &info) && S_ISDIR(info.st_mode) && !(info.st_mode & 0022),
          "GPU artifact root must be a private directory");
  Require(!captured_pids.empty() && !visible_devices.empty(), "GPU artifact requires participants and GPUs");
  std::set<uint32_t> pids;
  for (const auto pid : captured_pids)
    Require(pid > 0 && pid <= INT_MAX && pids.insert(pid).second, "invalid or duplicate captured GPU PID");
  std::set<std::string> visible;
  for (const auto& uuid : visible_devices) {
    std::string canonical;
    Require(storage::CanonicalizeGPUUUID(uuid, &canonical) && visible.insert(canonical).second,
            "invalid or duplicate visible GPU UUID");
    impl_->visible_devices.push_back(std::move(canonical));
  }
  std::set<std::string> sources, destinations;
  for (const auto& pair : device_map) {
    std::array<unsigned char, 16> source{}, destination{};
    Require(storage::ParseGPUUUID(pair.source_uuid, &source) && storage::ParseGPUUUID(pair.target_uuid, &destination),
            "invalid GPU device map");
    const auto old_uuid = storage::FormatGPUUUID(source), new_uuid = storage::FormatGPUUUID(destination);
    Require(sources.insert(old_uuid).second && destinations.insert(new_uuid).second && visible.contains(new_uuid),
            "GPU device map must be one-to-one and target visible GPUs");
    CUcheckpointGpuPair gpu{};
    std::copy(source.begin(), source.end(), gpu.oldUuid.bytes);
    std::copy(destination.begin(), destination.end(), gpu.newUuid.bytes);
    impl_->pairs.push_back(gpu);
    impl_->mapping.push_back({old_uuid, new_uuid});
  }
  const bool save = direction == Direction::Checkpoint;
  Require(!save || device_map.empty(), "checkpoint cannot remap GPUs");
  if (save) Require(mkdirat(impl_->root.get(), "native", 0700) == 0 || errno == EEXIST, "create GPU root");
  FileDescriptor gpu_root(openat(impl_->root.get(), "native", O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW));
  Require(gpu_root.get() >= 0, "open GPU root");
  for (const auto pid : captured_pids) {
    const auto name = std::to_string(pid);
    if (save) Require(mkdirat(gpu_root.get(), name.c_str(), 0700) == 0, "create GPU participant directory");
    FileDescriptor directory(openat(gpu_root.get(), name.c_str(), O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW));
    Require(directory.get() >= 0 && !fstat(directory.get(), &info) && !(info.st_mode & 0022),
            "GPU participant directory must be private");
    PreparedParticipant participant{pid, std::move(directory), {}};
    if (!save) {
      const auto path = "/proc/self/fd/" + std::to_string(participant.directory.get());
      std::string error;
      if (!storage::ReadManifest(path, &participant.manifest, &error) ||
          !storage::ValidateExtentFiles(path, participant.manifest, &error)) throw std::runtime_error(error);
      for (const auto& extent : participant.manifest) {
        const auto mapped = std::find_if(impl_->mapping.begin(), impl_->mapping.end(),
            [&](const auto& pair) { return pair.source_uuid == extent.source_uuid; });
        const auto& target = mapped == impl_->mapping.end() ? extent.source_uuid : mapped->destination_uuid;
        Require(visible.contains(target), "GPU artifact refers to an unavailable target GPU");
      }
    }
    impl_->participants.push_back(std::move(participant));
  }
}
Artifact::~Artifact() = default;

struct GpuEngine::Impl : Resources { using Resources::Resources; };
GpuEngine::GpuEngine(EngineOptions options) : impl_(std::make_unique<Impl>(options)) {}
GpuEngine::~GpuEngine() {
  // After a fatal error, DMA can still use these resources. Keep them until
  // the daemon exits.
  if (impl_ && impl_->fatal.load()) (void)impl_.release();
}
bool GpuEngine::Available() const { return impl_->available && !impl_->fatal.load(); }
std::vector<ParticipantResult> GpuEngine::Checkpoint(Artifact& artifact, const std::vector<Participant>& participants,
                                                 Cancellation& cancellation) {
  return Execute(artifact, participants, cancellation, Direction::Checkpoint);
}
std::vector<ParticipantResult> GpuEngine::Restore(Artifact& artifact, const std::vector<Participant>& participants,
                                Cancellation& cancellation) {
  return Execute(artifact, participants, cancellation, Direction::Restore);
}
std::vector<ParticipantResult> GpuEngine::Execute(Artifact& artifact, const std::vector<Participant>& participants,
                                Cancellation& cancellation, Direction direction) {
  if (impl_->fatal.load()) throw FatalError("GPU engine is unavailable after a fatal error");
  Require(Available(), "CustomStorage is unavailable");
  auto& prepared = *artifact.impl_;
  Require(prepared.direction == direction, "GPU operation does not match artifact direction");
  Require(participants.size() == prepared.participants.size(), "GPU participant set mismatch");
  std::map<uint32_t, uint32_t> targets;
  std::set<uint32_t> target_pids;
  for (const auto& participant : participants)
    Require(participant.target_pid > 0 && participant.target_pid <= INT_MAX &&
            targets.emplace(participant.captured_pid, participant.target_pid).second &&
            target_pids.insert(participant.target_pid).second, "invalid or duplicate GPU target");
  for (const auto& participant : prepared.participants)
    Require(targets.contains(participant.captured_pid), "missing GPU participant target");
  for (const auto& uuid : prepared.visible_devices)
    Require(std::any_of(impl_->devices.begin(), impl_->devices.end(),
                       [&](const auto& device) { return device.second->uuid == uuid; }), "target GPU is unavailable");
  CheckCancellation(cancellation);
  // Claim targets before consuming the artifact or entering failure cleanup.
  std::vector<int> host_pids(target_pids.begin(), target_pids.end());
  TargetLease target_lease(*impl_, std::move(host_pids));
  Require(!prepared.consumed.exchange(true), "GPU artifact operation already executed");
  Cancellation batch_cancellation(&cancellation);
  const bool save = direction == Direction::Checkpoint;
  std::vector<ParticipantResult> result(participants.size());
  std::vector<std::unique_ptr<Operation>> operations;
  std::vector<std::jthread> transfers;
  operations.reserve(participants.size());
  transfers.reserve(participants.size());
  std::mutex failure_mutex;
  std::exception_ptr failure;
  bool fatal = false;
  auto fail = [&] {
    const auto current = std::current_exception();
    bool is_fatal = false;
    try {
      std::rethrow_exception(current);
    } catch (const FatalError&) {
      is_fatal = true;
      LogCurrentException("GPU operation failed");
    } catch (...) {}
    std::lock_guard lock(failure_mutex);
    if (!failure || is_fatal) failure = current;
    fatal = fatal || is_fatal;
    batch_cancellation.Cancel();
  };
  auto transfer = [&](size_t index) {
    transfers.emplace_back([&, index] { try { operations[index]->Transfer(); } catch (...) { fail(); } });
  };
  try {
    for (size_t index = 0; index < prepared.participants.size(); ++index) {
      auto& participant = prepared.participants[index];
      operations.push_back(std::make_unique<Operation>(*impl_, participant,
          targets.at(participant.captured_pid), save, prepared.mapping, prepared.pairs, batch_cancellation,
          result[index]));
    }
    if (save) for (auto& operation : operations) { CheckCancellation(batch_cancellation); operation->Lock(); }
    for (size_t index = 0; index < operations.size(); ++index) {
      operations[index]->Prepare();
      // Restore transfer i overlaps preparation i+1. Checkpoint transfers start
      // only after every participant is prepared.
      if (!save) transfer(index);
    }
    if (save) for (size_t index = 0; index < operations.size(); ++index) transfer(index);
  } catch (...) { fail(); }
  for (auto& thread : transfers) thread.join();
  if (!failure) {
    try {
      CheckCancellation(batch_cancellation);
      for (auto operation = operations.rbegin(); operation != operations.rend(); ++operation) (*operation)->Complete();
      if (save) for (auto& operation : operations) operation->WriteManifest();
      CheckCancellation(batch_cancellation);
      // No participant resumes until every Complete succeeds. Cancellation
      // arriving after this point must not interrupt the unlock sequence.
      if (!save) for (auto operation = operations.rbegin(); operation != operations.rend(); ++operation) (*operation)->Unlock();
    } catch (...) { fail(); }
  }
  if (failure) {
    if (!fatal) {
      // Killing a PID namespace init also kills its children. Complete every
      // participant while all targets are alive, then kill the targets.
      for (auto& operation : operations) {
        try { operation->Complete(); } catch (...) {
          fatal = true;
          impl_->fatal.store(true);
          LogCurrentException("GPU completion cleanup failed");
        }
      }
    }
    if (fatal) impl_->fatal.store(true);
    for (auto& operation : operations) {
      try { operation->Terminate(); } catch (...) {
        fatal = true;
        impl_->fatal.store(true);
        LogCurrentException("GPU target cleanup failed");
      }
    }
    if (fatal) {
      impl_->fatal.store(true);
      // Keep file descriptors and mappings until the daemon exits.
      for (auto& operation : operations) (void)operation.release();
      throw FatalError("GPU cleanup failed");
    }
    std::rethrow_exception(failure);
  }
  return result;
}
}  // namespace snapshot::pagebroker::gpu

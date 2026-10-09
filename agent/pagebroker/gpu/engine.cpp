// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0
#include "engine.hpp"

#include <nvtx3/nvtx3.hpp>
#include <fcntl.h>
#include <charconv>
#include <filesystem>
#include <future>
#include <sys/stat.h>
#include <unistd.h>
#include <algorithm>
#include <array>
#include <atomic>
#include <cerrno>
#include <chrono>
#include <climits>
#include <cstdio>
#include <cstdlib>
#include <exception>
#include <iterator>
#include <limits>
#include <map>
#include <mutex>
#include <set>
#include <system_error>
#include <thread>
#include <utility>

#include "checkpoint.hpp"
#include "../errors.hpp"
#include "file_descriptor.hpp"
#include "storage_manifest.hpp"
#include "transfer.hpp"

namespace snapshot::pagebroker::gpu {
namespace {
namespace transfer = snapshot::pagebroker::cuda;
constexpr auto kGpuWaitInterval = std::chrono::milliseconds{100};

FileDescriptor
OpenDirectory(int parent, const char* name)
{
  FileDescriptor directory(openat(parent, name, O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW));
  if (directory.get() < 0) {
    throw std::system_error(errno, std::generic_category(), "open GPU directory");
  }
  return directory;
}

void
ValidateDirectory(int fd)
{
  struct stat info{};
  if (fstat(fd, &info)) {
    throw std::system_error(errno, std::generic_category(), "stat GPU directory");
  }
  Validate(S_ISDIR(info.st_mode), "GPU artifact path must be a directory");
  Validate(!(info.st_mode & (S_IWGRP | S_IWOTH)),
           "GPU directory must not be writable by group or others");
}

struct PrimaryContext {
  explicit PrimaryContext(CUdevice device) : device(device)
  {
    CheckCuda(cuDevicePrimaryCtxRetain(&handle, device), "retain PageBroker context");
  }
  ~PrimaryContext()
  {
    const auto result = cuDevicePrimaryCtxRelease(device);
    if (result != CUDA_SUCCESS) {
      std::fprintf(stderr, "release PageBroker context: CUDA error %d\n", static_cast<int>(result));
    }
  }
  PrimaryContext(const PrimaryContext&) = delete;
  PrimaryContext& operator=(const PrimaryContext&) = delete;

  CUdevice device;
  CUcontext handle = nullptr;
};

struct Device {
  Device(CUdevice device, transfer::TransferOptions options)
      : context(device), buffers(options)
  {
    CUuuid device_uuid;
    CheckCuda(cuDeviceGetUuid(&device_uuid, device), "get GPU UUID");
    std::array<unsigned char, 16> bytes{};
    std::copy(std::begin(device_uuid.bytes), std::end(device_uuid.bytes), bytes.begin());
    uuid = storage::FormatGPUUUID(bytes);
  }

  // Member order releases buffers before the retained context, including
  // partial engine setup and failed insertion into the device map.
  PrimaryContext context;
  std::string uuid;
  std::timed_mutex mutex;
  transfer::TransferBuffers buffers;
};

class TargetClaim {
public:
  TargetClaim(std::mutex& mutex, std::set<int>& active, std::vector<int> targets)
      : mutex_(mutex), active_(active), targets_(std::move(targets))
  {
    std::lock_guard lock(mutex_);
    for (const auto target : targets_) {
      if (active_.contains(target)) {
        throw TargetConflict("GPU target belongs to another active transaction");
      }
    }
    size_t claimed = 0;
    try {
      for (const auto target : targets_) {
        active_.insert(target);
        ++claimed;
      }
    } catch (...) {
      for (size_t index = 0; index < claimed; ++index) {
        active_.erase(targets_[index]);
      }
      throw;
    }
  }
  ~TargetClaim()
  {
    std::lock_guard lock(mutex_);
    for (const auto target : targets_) {
      active_.erase(target);
    }
  }

private:
  std::mutex& mutex_;
  std::set<int>& active_;
  std::vector<int> targets_;
};

struct PreparedParticipant {
  uint32_t captured_pid;
  FileDescriptor directory;
  std::vector<storage::GpuDataFile> manifest;
  std::vector<std::shared_ptr<transfer::TransferFile>> files;
};
} // namespace

void
ValidateParticipants(std::span<const uint32_t> captured_pids, std::span<const Participant> participants)
{
  Validate(!captured_pids.empty() && captured_pids.size() == participants.size(),
           "GPU participant set mismatch");
  std::set<uint32_t> captured;
  for (const auto pid : captured_pids) {
    Validate(pid > 0 && pid <= INT_MAX && captured.insert(pid).second,
             "invalid or duplicate captured GPU PID");
  }
  std::set<uint32_t> targets;
  for (const auto& participant : participants) {
    Validate(participant.target_pid > 0 && participant.target_pid <= INT_MAX &&
                 captured.erase(participant.captured_pid) && targets.insert(participant.target_pid).second,
             "invalid or duplicate GPU target");
  }
}

struct RestorePreparation::State {
  using Files = std::map<std::pair<uint32_t, std::string>, std::shared_ptr<transfer::TransferFile>>;
  std::shared_ptr<std::atomic<bool>> cancelled = std::make_shared<std::atomic<bool>>(false);
  // Declared last so destruction joins before releasing the cancellation token.
  std::shared_future<Files> pending;

  State(int directory_fd, size_t lanes)
  {
    pending = std::async(std::launch::async,
        [root = FileDescriptor::Duplicate(directory_fd), lanes, cancelled = cancelled] {
      const nvtx3::scoped_range range{"PageBroker restore file preparation"};
      Files files;
      ValidateDirectory(root.get());
      FileDescriptor gpu_root(openat(root.get(), kDataDirectory, O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW));
      if (gpu_root.get() < 0 && errno == ENOENT) {
        return files;  // CPU-only checkpoint.
      }
      if (gpu_root.get() < 0) {
        throw std::system_error(errno, std::generic_category(), "open GPU preparation directory");
      }
      ValidateDirectory(gpu_root.get());
      const auto path = "/proc/self/fd/" + std::to_string(gpu_root.get());
      for (const auto& entry : std::filesystem::directory_iterator(path)) {
        if (cancelled->load()) return Files{};
        const auto name = entry.path().filename().string();
        uint32_t pid = 0;
        const auto [end, error] = std::from_chars(name.data(), name.data() + name.size(), pid);
        Validate(error == std::errc{} && end == name.data() + name.size() && pid > 0 &&
                 pid <= INT_MAX && name == std::to_string(pid), "invalid GPU participant directory");
        auto directory = OpenDirectory(gpu_root.get(), name.c_str());
        ValidateDirectory(directory.get());
        std::vector<storage::GpuDataFile> manifest;
        std::string message;
        const auto participant_path = "/proc/self/fd/" + std::to_string(directory.get());
        if (!storage::ReadManifest(participant_path, &manifest, &message) ||
            !storage::ValidateExtentFiles(participant_path, manifest, &message)) {
          throw std::runtime_error(message);
        }
        for (const auto& extent : manifest) {
          if (cancelled->load()) return Files{};
          FileDescriptor file(openat(directory.get(), extent.filename.c_str(),
                                     O_RDONLY | O_CLOEXEC | O_NOFOLLOW | O_DIRECT));
          if (file.get() < 0) {
            throw std::system_error(errno, std::generic_category(), "prepare GPU extent");
          }
          files.emplace(std::make_pair(pid, extent.filename),
                        std::make_shared<transfer::TransferFile>(file.get(), lanes));
        }
      }
      return files;
    });
  }
  ~State() { cancelled->store(true); }

  std::shared_ptr<transfer::TransferFile> Get(uint32_t pid, const storage::GpuDataFile& extent, int directory)
  {
    const nvtx3::scoped_range range{"PageBroker restore preparation wait"};
    const auto file = pending.get().at({pid, extent.filename});
    struct stat held{}, current{};
    Validate(fstat(file->fd(), &held) == 0 &&
             fstatat(directory, extent.filename.c_str(), &current, AT_SYMLINK_NOFOLLOW) == 0 &&
             S_ISREG(held.st_mode) && held.st_dev == current.st_dev && held.st_ino == current.st_ino &&
             held.st_size >= 0 && static_cast<uint64_t>(held.st_size) == extent.size,
             "GPU extent changed after preparation");
    return file;
  }
};

RestorePreparation::RestorePreparation(int directory_fd, size_t lanes)
    : state_(std::make_unique<State>(directory_fd, lanes)) {}
RestorePreparation::~RestorePreparation() = default;

struct Artifact::ArtifactState {
  ArtifactState(int directory_fd, Direction direction)
      : root(FileDescriptor::Duplicate(directory_fd)), direction(direction)
  {
    ValidateDirectory(root.get());
  }

  void CreateCheckpointDirectories(const std::vector<uint32_t>& captured_pids)
  {
    // Allocate names before mkdir so rollback needs no allocations.
    std::vector<std::string> names;
    names.reserve(captured_pids.size());
    for (const auto pid : captured_pids) {
      names.push_back(std::to_string(pid));
    }
    FileDescriptor gpu_root(-1);
    bool created_root = false;
    size_t created = 0;
    try {
      if (mkdirat(root.get(), kDataDirectory, S_IRWXU) == 0) {
        created_root = true;
      } else if (errno != EEXIST) {
        throw std::system_error(errno, std::generic_category(), "create GPU root directory");
      }
      gpu_root = OpenDirectory(root.get(), kDataDirectory);
      ValidateDirectory(gpu_root.get());
      for (size_t index = 0; index < names.size(); ++index) {
        if (mkdirat(gpu_root.get(), names[index].c_str(), S_IRWXU)) {
          throw std::system_error(errno, std::generic_category(), "create GPU participant directory");
        }
        ++created;
        auto directory = OpenDirectory(gpu_root.get(), names[index].c_str());
        ValidateDirectory(directory.get());
        participants.push_back({captured_pids[index], std::move(directory), {}, {}});
      }
    } catch (...) {
      for (size_t index = 0; index < created; ++index) {
        if (unlinkat(gpu_root.get(), names[index].c_str(), AT_REMOVEDIR)) {
          std::perror("remove incomplete GPU participant directory");
        }
      }
      if (created_root && unlinkat(root.get(), kDataDirectory, AT_REMOVEDIR)) {
        std::perror("remove incomplete GPU root directory");
      }
      throw;
    }
  }

  void OpenRestoreDirectories(const std::vector<uint32_t>& captured_pids)
  {
    auto gpu_root = OpenDirectory(root.get(), kDataDirectory);
    ValidateDirectory(gpu_root.get());
    for (const auto pid : captured_pids) {
      auto directory = OpenDirectory(gpu_root.get(), std::to_string(pid).c_str());
      ValidateDirectory(directory.get());
      PreparedParticipant participant{pid, std::move(directory), {}, {}};
      const auto path = "/proc/self/fd/" + std::to_string(participant.directory.get());
      std::string error;
      if (!storage::ReadManifest(path, &participant.manifest, &error) ||
          !storage::ValidateExtentFiles(path, participant.manifest, &error)) {
        throw std::runtime_error(error);
      }
      for (const auto& file : participant.manifest) {
        if (preparation) {
          participant.files.push_back(preparation->state_->Get(pid, file, participant.directory.get()));
        }
        const auto mapped = std::find_if(mapping.begin(), mapping.end(), [&](const auto& pair) {
          return pair.source_uuid == file.source_uuid;
        });
        const auto& target = mapped == mapping.end() ? file.source_uuid : mapped->destination_uuid;
        Validate(std::find(visible_devices.begin(), visible_devices.end(), target) != visible_devices.end(),
                 "GPU artifact refers to an unavailable target GPU");
      }
      participants.push_back(std::move(participant));
    }
  }

  FileDescriptor root;
  std::shared_ptr<RestorePreparation> preparation;
  Direction direction;
  std::vector<std::string> visible_devices;
  std::vector<storage::DevicePair> mapping;
  std::vector<CUcheckpointGpuPair> pairs;
  std::vector<PreparedParticipant> participants;
  std::atomic<bool> consumed{false};
};

Artifact::Artifact(int directory_fd, Direction direction, std::vector<uint32_t> captured_pids,
                   std::vector<std::string> visible_devices, std::vector<DeviceMapping> device_map,
                   std::shared_ptr<RestorePreparation> preparation)
    : state_(std::make_unique<ArtifactState>(directory_fd, direction))
{
  state_->preparation = std::move(preparation);
  Validate(direction == Direction::Checkpoint || direction == Direction::Restore, "invalid GPU operation direction");
  Validate(!captured_pids.empty() && !visible_devices.empty(), "GPU artifact requires participants and GPUs");
  std::set<uint32_t> pids;
  for (const auto pid : captured_pids) {
    Validate(pid > 0 && pid <= INT_MAX && pids.insert(pid).second, "invalid or duplicate captured GPU PID");
  }
  std::set<std::string> visible;
  for (const auto& uuid : visible_devices) {
    std::string canonical;
    Validate(storage::CanonicalizeGPUUUID(uuid, &canonical) && visible.insert(canonical).second,
             "invalid or duplicate visible GPU UUID");
    state_->visible_devices.push_back(std::move(canonical));
  }
  std::set<std::string> sources;
  std::set<std::string> destinations;
  for (const auto& pair : device_map) {
    std::array<unsigned char, 16> source{};
    std::array<unsigned char, 16> destination{};
    Validate(storage::ParseGPUUUID(pair.source_uuid, &source) &&
                 storage::ParseGPUUUID(pair.target_uuid, &destination), "invalid GPU device map");
    const auto old_uuid = storage::FormatGPUUUID(source);
    const auto new_uuid = storage::FormatGPUUUID(destination);
    Validate(sources.insert(old_uuid).second && destinations.insert(new_uuid).second && visible.contains(new_uuid),
             "GPU device map must be one-to-one and target visible GPUs");
    CUcheckpointGpuPair gpu{};
    std::copy(source.begin(), source.end(), gpu.oldUuid.bytes);
    std::copy(destination.begin(), destination.end(), gpu.newUuid.bytes);
    state_->pairs.push_back(gpu);
    state_->mapping.push_back({old_uuid, new_uuid});
  }
  state_->participants.reserve(captured_pids.size());
  switch (direction) {
    case Direction::Checkpoint:
      Validate(device_map.empty(), "checkpoint cannot remap GPUs");
      state_->CreateCheckpointDirectories(captured_pids);
      break;
    case Direction::Restore:
      state_->OpenRestoreDirectories(captured_pids);
      break;
  }
}

Artifact::~Artifact() = default;

struct GpuEngine::GpuEngineState {
  explicit GpuEngineState(const EngineOptions& options) : pooled_lanes(options.pooled_lanes)
  {
    Require(!std::getenv("CUDA_CHECKPOINT_JOB_FILE"), "CUDA_CHECKPOINT_JOB_FILE must be unset");
    if (!checkpoint.SupportsCustomStorage()) {
      return;
    }
    int count = 0;
    CheckCuda(cuDeviceGetCount(&count), "get GPU count");
    if (!count) {
      return;
    }
    const transfer::TransferOptions settings{options.buffer_count, options.chunk_bytes, true};
    transfer::TransferMemoryBytes(settings, count, options.max_pinned_bytes);
    for (int index = 0; index < count; ++index) {
      CUdevice id;
      CheckCuda(cuDeviceGet(&id, index), "get GPU device");
      devices.emplace(id, std::make_unique<Device>(id, settings));
    }
    size_t total_bytes = 0;
    for (const auto& [id, device] : devices) {
      (void)id;
      const size_t bytes = transfer::TransferBuffers::AllocationBytes(device->context.handle, settings);
      Require(bytes <= std::numeric_limits<size_t>::max() - total_bytes, "total pinned allocation overflow");
      total_bytes += bytes;
      Require(!options.max_pinned_bytes || total_bytes <= options.max_pinned_bytes,
              "total pinned allocation exceeds max-pinned-bytes");
    }
    if (options.pooled_lanes) {
      std::vector<CUcontext> contexts;
      for (const auto& [id, device] : devices) {
        (void)id;
        contexts.push_back(device->context.handle);
      }
      pool = std::make_unique<transfer::TransferPool>(contexts, settings, options.pooled_lanes);
    } else {
      for (const auto& [id, device] : devices) {
        (void)id;
        device->buffers.Initialize(device->context.handle);
      }
    }
    available = true;
  }

  Device& DeviceForContext(CUcontext context)
  {
    for (auto& [id, device] : devices) {
      (void)id;
      if (device->context.handle == context) {
        return *device;
      }
    }
    throw std::runtime_error("CustomStorage returned an unknown CUDA context");
  }

  void ValidateDevices(const std::vector<std::string>& uuids) const
  {
    for (const auto& uuid : uuids) {
      Validate(std::any_of(devices.begin(), devices.end(), [&](const auto& device) {
        return device.second->uuid == uuid;
      }), "target GPU is unavailable");
    }
  }

  driver::CheckpointAPI checkpoint;
  std::map<CUdevice, std::unique_ptr<Device>> devices;
  std::unique_ptr<transfer::TransferPool> pool;
  size_t pooled_lanes;
  bool available = false;
  std::mutex targets_mutex;
  std::set<int> active_targets;
};

std::unique_lock<std::timed_mutex>
AcquireDevice(std::timed_mutex& mutex, const Cancellation& cancellation)
{
  std::unique_lock lock(mutex, std::defer_lock);
  do {
    cancellation.ThrowIfCancelled();
  } while (!lock.try_lock_for(kGpuWaitInterval));
  cancellation.ThrowIfCancelled();
  return lock;
}

class GpuEngine::Batch {
  class Operation {
  public:
    Operation(GpuEngineState& engine, PreparedParticipant& prepared, const Participant& target,
              const Cancellation& cancellation, ParticipantResult& result)
        : engine_(engine), prepared_(prepared), cancellation_(cancellation), result_(result),
          cuda_(engine.checkpoint, static_cast<int>(target.target_pid), FileDescriptor::Duplicate(target.pidfd))
    {
      result_.captured_pid = prepared.captured_pid;
    }

    void CheckTarget() const
    {
      cuda_.CheckTarget();
    }

    void Lock()
    {
      const nvtx3::scoped_range range{"PageBroker checkpoint lock"};
      cuda_.Lock();
    }

    void PrepareCheckpoint()
    {
      const nvtx3::scoped_range range{"PageBroker CUDA setup", nvtx3::payload{prepared_.captured_pid}};
      view_ = cuda_.PrepareCheckpoint();
      const auto regions = Regions();
      std::string error;
      if (!storage::BuildCheckpointManifest(regions, &prepared_.manifest, &error) ||
          !storage::BuildTransferJobs(prepared_.manifest, regions, {}, &jobs_, &error)) {
        throw std::runtime_error(error);
      }
    }

    void PrepareRestore(std::span<CUcheckpointGpuPair> pairs, const std::vector<storage::DevicePair>& mapping)
    {
      const nvtx3::scoped_range range{"PageBroker CUDA setup", nvtx3::payload{prepared_.captured_pid}};
      view_ = cuda_.PrepareRestore(pairs);
      const auto regions = Regions();
      std::string error;
      if (!storage::BuildTransferJobs(prepared_.manifest, regions, mapping, &jobs_, &error)) {
        throw std::runtime_error(error);
      }
    }

    void Checkpoint()
    {
      const nvtx3::scoped_range range{"PageBroker participant transfer", nvtx3::payload{prepared_.captured_pid}};
      for (const auto& job : jobs_) {
        auto transfer = PrepareTransfer(job);
        const auto& data = transfer.data;
        const auto& name = transfer.name;
        FileDescriptor file(openat(prepared_.directory.get(), name.c_str(),
                                   O_CLOEXEC | O_NOFOLLOW | O_DIRECT | O_CREAT | O_EXCL | O_RDWR,
                                   S_IRUSR | S_IWUSR));
        if (file.get() < 0) {
          throw std::system_error(errno, std::generic_category(), "create GPU data file");
        }
        if (ftruncate(file.get(), data.size)) {
          throw std::system_error(errno, std::generic_category(), "size GPU data file");
        }
        if (engine_.pool) {
          engine_.pool->Checkpoint(file.get(), transfer.device.context.handle, data.devPtr,
                                 data.size, data.stream, cancellation_);
        } else {
          transfer.device.buffers.Checkpoint(file.get(), data.devPtr, data.size, data.stream, cancellation_);
        }
        result_.bytes += data.size;
      }
    }

    void Restore()
    {
      const nvtx3::scoped_range range{"PageBroker participant transfer", nvtx3::payload{prepared_.captured_pid}};
      for (const auto& job : jobs_) {
        auto transfer = PrepareTransfer(job);
        const auto& data = transfer.data;
        const auto& name = transfer.name;
        if (engine_.pool && !prepared_.files.empty()) {
          engine_.pool->Restore(*prepared_.files.at(job.extent_index), transfer.device.context.handle,
                                data.devPtr, data.size, data.stream, cancellation_);
          result_.bytes += data.size;
          continue;
        }
        FileDescriptor file(openat(prepared_.directory.get(), name.c_str(), O_CLOEXEC | O_NOFOLLOW | O_DIRECT | O_RDONLY));
        if (file.get() < 0) {
          throw std::system_error(errno, std::generic_category(), "open GPU data file");
        }
        if (engine_.pool) {
          engine_.pool->Restore(file.get(), transfer.device.context.handle, data.devPtr,
                                 data.size, data.stream, cancellation_);
        } else {
          transfer.device.buffers.Restore(file.get(), data.devPtr, data.size, data.stream, cancellation_);
        }
        result_.bytes += data.size;
      }
    }

    void Complete()
    {
      const nvtx3::scoped_range range{"PageBroker CUDA completion", nvtx3::payload{prepared_.captured_pid}};
      cuda_.Complete();
    }

    void WriteManifest()
    {
      std::string error;
      const auto directory = "/proc/self/fd/" + std::to_string(prepared_.directory.get());
      if (!storage::WriteManifest(directory, prepared_.manifest, &error)) {
        throw std::runtime_error(error);
      }
    }

    void Unlock()
    {
      const nvtx3::scoped_range range{"PageBroker CUDA unlock", nvtx3::payload{prepared_.captured_pid}};
      cuda_.Unlock();
    }

    void Abort()
    {
      cuda_.Abort();
    }

  private:
    std::vector<storage::GpuDataRegion> Regions()
    {
      // CustomStorage requires retained primary contexts on all participating
      // GPUs. Each returned stream identifies its own GPU context. Preparation
      // does not require selecting the first GPU as the current context.
      // https://docs.nvidia.com/cuda/cuda-driver-api/cuda_driver_api/group__CUDA__CHECKPOINT.html
      Require(view_->deviceCount <= engine_.devices.size(), "too many CustomStorage devices");
      std::vector<storage::GpuDataRegion> regions;
      for (unsigned index = 0; index < view_->deviceCount; ++index) {
        CUcontext context = nullptr;
        CheckCuda(cuStreamGetCtx(view_->perDeviceData[index].stream, &context), "get CustomStorage stream context");
        auto& device = engine_.DeviceForContext(context);
        owners_.push_back(&device);
        regions.push_back({device.uuid, view_->perDeviceData[index].size});
      }
      return regions;
    }

    struct Transfer {
      Device& device;
      std::unique_lock<std::timed_mutex> lease;
      const CUcheckpointCustomStoragePerDeviceData& data;
      const std::string& name;
    };

    Transfer PrepareTransfer(const storage::TransferJob& job)
    {
      auto& device = *owners_[job.device_index];
      return {device, AcquireDevice(device.mutex, cancellation_),
              view_->perDeviceData[job.device_index], prepared_.manifest[job.extent_index].filename};
    }

    GpuEngineState& engine_;
    PreparedParticipant& prepared_;
    const Cancellation& cancellation_;
    ParticipantResult& result_;
    driver::Operation cuda_;
    const CUcheckpointCustomStorageInfo* view_ = nullptr;
    std::vector<Device*> owners_;
    std::vector<storage::TransferJob> jobs_;
  };

public:
  Batch(GpuEngineState& engine, Artifact::ArtifactState& artifact,
        const std::vector<Participant>& participants, Cancellation& cancellation, Direction direction)
      : artifact_(artifact), cancellation_(&cancellation), results_(participants.size())
  {
    Require(engine.available, "CustomStorage is unavailable");
    Validate(artifact.direction == direction, "GPU operation does not match artifact direction");
    std::vector<uint32_t> captured_pids;
    for (const auto& participant : artifact.participants) {
      captured_pids.push_back(participant.captured_pid);
    }
    ValidateParticipants(captured_pids, participants);
    engine.ValidateDevices(artifact.visible_devices);
    std::map<uint32_t, Participant> targets;
    std::vector<int> host_pids;
    for (const auto& participant : participants) {
      targets.emplace(participant.captured_pid, participant);
      host_pids.push_back(static_cast<int>(participant.target_pid));
    }
    operations_.reserve(participants.size());
    transfers_.reserve(participants.size());
    for (size_t index = 0; index < artifact.participants.size(); ++index) {
      auto& participant = artifact.participants[index];
      operations_.push_back(std::make_unique<Operation>(engine, participant,
          targets.at(participant.captured_pid), cancellation_, results_[index]));
    }
    cancellation_.ThrowIfCancelled();
    claim_ = std::make_unique<TargetClaim>(engine.targets_mutex, engine.active_targets, std::move(host_pids));
  }

  std::vector<ParticipantResult> Checkpoint()
  {
    try {
      for (auto& operation : operations_) {
        Begin(*operation);
        operation->Lock();
      }
      for (auto& operation : operations_) {
        Begin(*operation);
        operation->PrepareCheckpoint();
      }
      for (auto& operation : operations_) {
        StartTransfer(*operation, &Operation::Checkpoint);
      }
    } catch (...) {
      RecordFailure(std::current_exception());
    }
    JoinTransfers();
    if (!failure_) {
      try {
        CompleteAll();
        for (auto& operation : operations_) {
          operation->WriteManifest();
        }
        cancellation_.ThrowIfCancelled();
      } catch (...) {
        RecordFailure(std::current_exception());
      }
    }
    return Finish();
  }

  std::vector<ParticipantResult> Restore()
  {
    try {
      for (auto& operation : operations_) {
        Begin(*operation);
        operation->PrepareRestore(artifact_.pairs, artifact_.mapping);
        // Transfer i runs while preparation i+1 sets up its CUDA mappings.
        StartTransfer(*operation, &Operation::Restore);
      }
    } catch (...) {
      RecordFailure(std::current_exception());
    }
    JoinTransfers();
    if (!failure_) {
      try {
        CompleteAll();
        cancellation_.ThrowIfCancelled();
        // CUDA unlocks one process at a time. Ignore cancellation once this
        // sequence starts. A later unlock failure fails the whole restore and
        // cleanup terminates its targets, including any that already resumed.
        for (auto operation = operations_.rbegin(); operation != operations_.rend(); ++operation) {
          (*operation)->Unlock();
        }
      } catch (...) {
        RecordFailure(std::current_exception());
      }
    }
    return Finish();
  }

private:
  void Begin(Operation& operation)
  {
    cancellation_.ThrowIfCancelled();
    operation.CheckTarget();
    if (!started_) {
      Require(!artifact_.consumed.exchange(true), "GPU artifact operation already executed");
      started_ = true;
    }
  }

  void RecordFailure(std::exception_ptr exception)
  {
    bool fatal = false;
    try {
      std::rethrow_exception(exception);
    } catch (const FatalError&) {
      SignalFatalCleanup();
      fatal = true;
    } catch (...) {
    }
    std::lock_guard lock(failure_mutex_);
    if (!failure_ || (fatal && !fatal_)) {
      failure_ = exception;
    }
    fatal_ = fatal_ || fatal;
    cancellation_.Cancel();
  }

  void StartTransfer(Operation& operation, void (Operation::*transfer)())
  {
    cancellation_.ThrowIfCancelled();
    transfers_.emplace_back([this, &operation, transfer] {
      try {
        (operation.*transfer)();
      } catch (...) {
        RecordFailure(std::current_exception());
      }
    });
  }

  void JoinTransfers()
  {
    for (auto& thread : transfers_) {
      thread.join();
    }
  }

  void CompleteAll()
  {
    cancellation_.ThrowIfCancelled();
    for (auto operation = operations_.rbegin(); operation != operations_.rend(); ++operation) {
      (*operation)->Complete();
    }
  }

  bool Cleanup()
  {
    // Namespace init must stay alive while children still have CUDA mappings.
    bool complete = true;
    for (auto& operation : operations_) {
      try {
        operation->Complete();
      } catch (...) {
        SignalFatalCleanup();
        LogException("GPU completion cleanup failed", std::current_exception());
        complete = false;
      }
    }
    if (!complete) {
      return false;
    }
    bool stopped = true;
    for (auto& operation : operations_) {
      try {
        operation->Abort();
      } catch (...) {
        SignalFatalCleanup();
        LogException("GPU target cleanup failed", std::current_exception());
        stopped = false;
      }
    }
    return stopped;
  }

  std::vector<ParticipantResult> Finish()
  {
    if (failure_) {
      // A fatal participant does not prevent completing the others. Cleanup
      // still keeps all targets alive if any imported mappings remain unsafe.
      const bool cleaned = Cleanup();
      if (fatal_ || !cleaned) {
        LogException("GPU operation cannot release resources", failure_);
        ReportFatalCleanup("GPU operation cannot release resources");
      }
      std::rethrow_exception(failure_);
    }
    return std::move(results_);
  }

  Artifact::ArtifactState& artifact_;
  Cancellation cancellation_;
  std::vector<ParticipantResult> results_;
  std::vector<std::unique_ptr<Operation>> operations_;
  std::vector<std::jthread> transfers_;
  std::unique_ptr<TargetClaim> claim_;
  std::mutex failure_mutex_;
  std::exception_ptr failure_;
  bool fatal_ = false;
  bool started_ = false;
};

GpuEngine::GpuEngine(EngineOptions options) : state_(std::make_unique<GpuEngineState>(options))
{
}
GpuEngine::~GpuEngine() = default;

bool
GpuEngine::Available() const
{
  return state_->available;
}

std::shared_ptr<RestorePreparation>
GpuEngine::PrepareRestore(int directory_fd)
{
  if (!state_->available || !state_->pool) return {};
  return std::make_shared<RestorePreparation>(directory_fd, state_->pooled_lanes);
}

std::vector<ParticipantResult>
GpuEngine::Checkpoint(Artifact& artifact, const std::vector<Participant>& participants, Cancellation& cancellation)
{
  Batch batch(*state_, *artifact.state_, participants, cancellation, Direction::Checkpoint);
  return batch.Checkpoint();
}

std::vector<ParticipantResult>
GpuEngine::Restore(Artifact& artifact, const std::vector<Participant>& participants, Cancellation& cancellation)
{
  Batch batch(*state_, *artifact.state_, participants, cancellation, Direction::Restore);
  return batch.Restore();
}
} // namespace snapshot::pagebroker::gpu

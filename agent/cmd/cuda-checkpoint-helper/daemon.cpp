// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0
#include <fcntl.h>
#include <poll.h>
#include <signal.h>
#include <sys/eventfd.h>
#include <sys/socket.h>
#include <sys/syscall.h>
#include <sys/stat.h>
#include <sys/ioctl.h>
#include <linux/nsfs.h>
#include <unistd.h>

#include <algorithm>
#include <array>
#include <charconv>
#include <chrono>
#include <cstdlib>
#include <future>
#include <fstream>
#include <optional>
#include <sstream>
#include <iostream>
#include <limits>
#include <map>
#include <memory>
#include <mutex>
#include <stdexcept>
#include <string>
#include <string_view>
#include <syncstream>
#include <thread>
#include <vector>

#include "fd_transport.hpp"
#include "helper.pb.h"
#include "extent_digests.hpp"
#include "checkpoint.hpp"
#include "transfer.hpp"
#include "storage_manifest.hpp"

namespace transfer = snapshot::pagebroker::cuda;
namespace storage = cuda_checkpoint_storage;
namespace checkpoint = snapshot::cuda_checkpoint;
namespace protocol = snapshot::cuda_checkpoint::internal;
using namespace snapshot::pagebroker;
using Clock = std::chrono::steady_clock;

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
void
Require(bool ok, const char* message)
{
  if (!ok)
    throw std::runtime_error(message);
}
double
Seconds(Clock::time_point start)
{
  return std::chrono::duration<double>(Clock::now() - start).count();
}
long long
Nanoseconds(Clock::time_point time)
{
  return std::chrono::duration_cast<std::chrono::nanoseconds>(time.time_since_epoch()).count();
}

void
WatchOwner(int control)
{
  // CLI subprocesses temporarily inherit the control socket, so its EOF alone
  // cannot prove that the agent is alive. SO_PEERCRED identifies the process
  // that created the socketpair, even while a CLI holds another copy. A pidfd
  // watches that process, not the Go runtime thread that happened to fork us.
  ucred peer{};
  socklen_t size = sizeof(peer);
  Require(getsockopt(control, SOL_SOCKET, SO_PEERCRED, &peer, &size) == 0 && size == sizeof(peer) &&
              peer.pid > 0 && peer.pid == getppid(), "CUDA helper control owner is not its parent");
  FileDescriptor owner(static_cast<int>(syscall(SYS_pidfd_open, peer.pid, 0)));
  Require(owner.get() >= 0 && peer.pid == getppid(), "CUDA helper owner exited during startup");
  std::thread([owner = std::move(owner)] {
    pollfd event{owner.get(), POLLIN, 0};
    int result;
    do {
      result = poll(&event, 1, -1);
    } while (result < 0 && errno == EINTR);
    // Owner loss or an unusable lifetime watcher makes every context unsafe
    // to retain. Process teardown also interrupts initialization or CUDA calls.
    std::_Exit(1);
  }).detach();
}
struct DaemonOptions {
  bool custom_storage = true;
  size_t buffer_count = 32;
  size_t chunk_bytes = 128ULL * 1024 * 1024;
  size_t max_pinned_bytes = 0;
};
DaemonOptions
ParseOptions(int argc, char** argv)
{
  DaemonOptions options;
  Require(argc % 2 == 1, "CUDA helper options require values");
  for (int index = 1; index < argc; index += 2) {
    const std::string_view option(argv[index]), value(argv[index + 1]);
    if (option == "--cuda-storage-mode") {
      Require(value == "custom" || value == "driver", "unknown CUDA storage mode");
      options.custom_storage = value == "custom";
      continue;
    }
    size_t number = 0;
    const auto [end, error] = std::from_chars(value.data(), value.data() + value.size(), number);
    Require(error == std::errc{} && end == value.data() + value.size(), "invalid GPU allocation option");
    if (option == "--transfer-buffer-count")
      options.buffer_count = number;
    else if (option == "--transfer-chunk-bytes")
      options.chunk_bytes = number;
    else if (option == "--max-pinned-bytes")
      options.max_pinned_bytes = number;
    else
      throw std::runtime_error("unknown CUDA helper option");
  }
  return options;
}

struct Device {
  explicit Device(transfer::TransferOptions options) : buffers(options)
  {
  }
  CUdevice device;
  CUcontext context = nullptr;
  std::string uuid;
  std::mutex mutex;
  transfer::TransferBuffers buffers;
};

// NSpid is interpreted relative to the namespace pinned before CRIU, including
// restored descendants. A sibling namespace never matches the pinned identity.
std::optional<uint32_t>
PinnedPid(const std::filesystem::path& process, const struct stat& pinned)
{
  FileDescriptor candidate(open((process / "ns/pid").c_str(), O_RDONLY | O_CLOEXEC));
  size_t depth = 0;
  while (true) {
    struct stat actual{};
    if (candidate.get() < 0 || fstat(candidate.get(), &actual))
      return std::nullopt;
    if (actual.st_dev == pinned.st_dev && actual.st_ino == pinned.st_ino)
      break;
    candidate = FileDescriptor(ioctl(candidate.get(), NS_GET_PARENT));
    ++depth;
  }
  std::ifstream status(process / "status");
  std::string line;
  while (std::getline(status, line)) {
    if (!line.starts_with("NSpid:"))
      continue;
    std::istringstream values(line.substr(6));
    std::vector<uint32_t> pids;
    uint32_t value;
    while (values >> value)
      pids.push_back(value);
    if (pids.size() > depth)
      return pids[pids.size() - depth - 1];
    return std::nullopt;
  }
  return std::nullopt;
}

class PidNamespace {
public:
  explicit PidNamespace(FileDescriptor descriptor) : descriptor_(std::move(descriptor))
  {
    Require(fstat(descriptor_.get(), &identity_) == 0, "stat target namespace");
  }
  bool
  Matches(int host_pid, uint32_t namespace_pid) const
  {
    return PinnedPid("/proc/" + std::to_string(host_pid), identity_) == namespace_pid;
  }
  int
  Resolve(uint32_t namespace_pid)
  {
    Require(namespace_pid > 0 && namespace_pid <= INT_MAX, "invalid target PID");
    std::lock_guard lock(mutex_);
    const auto cached = host_pids_.find(namespace_pid);
    if (cached != host_pids_.end() && cached->second > 0 && Matches(cached->second, namespace_pid))
      return cached->second;
    // A single scan serves all participants sharing the pinned namespace.
    // Validate cache hits again so PID reuse never substitutes another namespace.
    host_pids_.clear();
    for (const auto& entry : std::filesystem::directory_iterator("/proc")) {
      const auto name = entry.path().filename().string();
      if (name.find_first_not_of("0123456789") != std::string::npos)
        continue;
      const auto pid = PinnedPid(entry.path(), identity_);
      if (!pid)
        continue;
      const auto [slot, inserted] = host_pids_.emplace(*pid, std::stoi(name));
      if (!inserted)
        slot->second = -1;
    }
    const auto found = host_pids_.find(namespace_pid);
    Require(found != host_pids_.end(), "GPU target absent from pinned namespace");
    Require(found->second > 0, "ambiguous GPU target");
    return found->second;
  }

private:
  FileDescriptor descriptor_;
  struct stat identity_{};
  std::mutex mutex_;
  std::map<uint32_t, int> host_pids_;
};

// All supported transfer resources are warm before READY, regardless of the
// capture mode. The same persistent contexts and registered rings serve later
// CustomStorage restores; the driver-managed path does not use them.
class Engine {
public:
  explicit Engine(DaemonOptions options = {})
  {
    Require(!std::getenv("CUDA_CHECKPOINT_JOB_FILE"), "CUDA helper daemon requires no CUDA_CHECKPOINT_JOB_FILE");
    if (options.custom_storage)
      checkpoint.RequireCustomStorage();
    int count = 0;
    Check(cuDeviceGetCount(&count), "cuDeviceGetCount");
    for (int index = 0; index < count; ++index) {
      auto owner = std::make_unique<Device>(transfer::TransferOptions{options.buffer_count, options.chunk_bytes, true});
      Check(cuDeviceGet(&owner->device, index), "cuDeviceGet");
      CUuuid uuid;
      Check(cuDeviceGetUuid(&uuid, owner->device), "cuDeviceGetUuid");
      std::array<unsigned char, 16> bytes{};
      std::copy(std::begin(uuid.bytes), std::end(uuid.bytes), bytes.begin());
      owner->uuid = storage::FormatGPUUUID(bytes);
      devices.emplace(owner->device, std::move(owner));
    }
    Require(!devices.empty(), "CUDA helper daemon has no devices");
    size_t nominal_bytes = 0;
    std::string error;
    if (!transfer::ValidateTransferMemory({options.buffer_count, options.chunk_bytes, true}, devices.size(),
                                          checkpoint.SupportsCustomStorage() ? options.max_pinned_bytes : 0,
                                          &nominal_bytes, &error))
      throw std::runtime_error(error);
    // Driver-managed operations also need warm primary contexts, including on
    // older drivers that do not expose CustomStorage transfer resources.
    for (auto& [id, device] : devices) {
      (void)id;
      Check(cuDevicePrimaryCtxRetain(&device->context, device->device), "retain helper context");
    }
    if (checkpoint.SupportsCustomStorage()) {
      size_t total_bytes = 0;
      for (auto& [id, device] : devices) {
        (void)id;
        size_t bytes = 0;
        if (!transfer::TransferBuffers::AllocationBytes(
                device->context, {options.buffer_count, options.chunk_bytes, true}, &bytes, &error))
          throw std::runtime_error(error);
        Require(bytes <= std::numeric_limits<size_t>::max() - total_bytes, "total pinned allocation overflow");
        total_bytes += bytes;
        Require(!options.max_pinned_bytes || total_bytes <= options.max_pinned_bytes,
                "total pinned allocation exceeds max-pinned-bytes");
      }
      for (auto& [id, device] : devices) {
        (void)id;
        if (!device->buffers.Initialize(device->context, &error))
          throw std::runtime_error("initialize CUDA helper transfer ring: " + error);
      }
    }
    ready_metrics.set_initialization_seconds(Seconds(start_));
    ready_metrics.set_visible_devices(devices.size());
  }
  Device&
  DeviceForContext(CUcontext context)
  {
    for (auto& [id, device] : devices) {
      (void)id;
      if (device->context == context)
        return *device;
    }
    throw std::runtime_error("CustomStorage returned an unknown CUDA context");
  }
  std::shared_ptr<PidNamespace>
  PinNamespace(uint32_t container_pid)
  {
    Require(container_pid > 1 && container_pid <= INT_MAX, "invalid container PID");
    FileDescriptor descriptor(
        open(("/proc/" + std::to_string(container_pid) + "/ns/pid").c_str(), O_RDONLY | O_CLOEXEC));
    Require(descriptor.get() >= 0, "pin target PID namespace");
    struct stat identity{};
    Require(fstat(descriptor.get(), &identity) == 0, "stat target PID namespace");
    const auto key = std::make_pair(identity.st_dev, identity.st_ino);
    std::lock_guard lock(namespace_mutex_);
    std::erase_if(namespaces_, [](const auto& entry) { return entry.second.expired(); });
    auto& known = namespaces_[key];
    if (auto shared = known.lock())
      return shared;
    auto shared = std::make_shared<PidNamespace>(std::move(descriptor));
    known = shared;
    return shared;
  }
  std::map<CUdevice, std::unique_ptr<Device>> devices;
  Clock::time_point start_ = Clock::now();
  checkpoint::CheckpointAPI checkpoint;
  protocol::GPUSessionMetrics ready_metrics;

private:
  std::mutex namespace_mutex_;
  std::map<std::pair<dev_t, ino_t>, std::weak_ptr<PidNamespace>> namespaces_;
};

class Operation {
public:
  Operation(Engine& engine, const protocol::BindGPUSession& binding, int host_pid, FileDescriptor directory,
            transfer::TransferCancellation& cancellation, std::vector<storage::ManifestExtent> manifest = {},
            std::vector<std::string> digests = {})
      : engine_(engine), directory_fd_(std::move(directory)),
        directory_("/proc/self/fd/" + std::to_string(directory_fd_.get())),
        save_(binding.direction() == protocol::BindGPUSession::SAVE),
        custom_storage_(binding.storage_mode() == protocol::BindGPUSession::CUSTOM_STORAGE),
        enable_checksum_digest_(binding.enable_checksum_digest()), cancellation_(cancellation),
        extent_digests_(std::move(digests)), cuda_(engine.checkpoint, host_pid), manifest_(std::move(manifest))
  {
    Require(binding.storage_mode() == protocol::BindGPUSession::CUSTOM_STORAGE ||
                binding.storage_mode() == protocol::BindGPUSession::DRIVER_MANAGED,
            "unknown GPU storage mode");
    Require(custom_storage_ || !enable_checksum_digest_,
            "checksum digests are only supported for CustomStorage extents");
    struct stat info{};
    Require(!fstat(directory_fd_.get(), &info) && S_ISDIR(info.st_mode) && !(info.st_mode & 0022),
            "GPU storage must be a private directory");
    for (const auto& uuid : binding.visible_devices()) {
      Require(std::any_of(engine_.devices.begin(), engine_.devices.end(),
                          [&](const auto& device) { return device.second->uuid == uuid; }),
              "target GPU is not available in the persistent engine");
    }
    const std::string& mapping = binding.device_map();
    size_t offset = 0;
    while (offset < mapping.size()) {
      const auto end = mapping.find(',', offset);
      const auto pair = mapping.substr(offset, end - offset);
      const auto equal = pair.find('=');
      std::array<unsigned char, 16> source{}, destination{};
      Require(equal != std::string::npos && storage::ParseGPUUUID(pair.substr(0, equal), &source) &&
                  storage::ParseGPUUUID(pair.substr(equal + 1), &destination),
              "invalid GPU device map");
      CUcheckpointGpuPair gpu{};
      std::copy(source.begin(), source.end(), gpu.oldUuid.bytes);
      std::copy(destination.begin(), destination.end(), gpu.newUuid.bytes);
      gpu_pairs_.push_back(gpu);
      device_pairs_.push_back({pair.substr(0, equal), pair.substr(equal + 1)});
      if (end == std::string::npos)
        break;
      offset = end + 1;
    }
    if (custom_storage_) {
      engine_.checkpoint.RequireCustomStorage();
      Check(cuCtxSetCurrent(engine_.devices.begin()->second->context), "set engine session context");
    }
  }

  protocol::GPUSessionReply
  Execute(const protocol::GPUSessionRequest& request)
  {
    using Op = protocol::GPUSessionRequest;
    using Reply = protocol::GPUSessionReply;
    const auto next = phase_ == Op::UNSPECIFIED ? (save_ ? Op::LOCK : Op::PREPARE)
                      : phase_ == Op::LOCK      ? Op::PREPARE
                      : phase_ == Op::PREPARE   ? Op::TRANSFER
                                                : Op::COMPLETE;
    Require(phase_ != Op::COMPLETE && request.operation() == next, "invalid GPU phase order");
    Require(!cancellation_.IsCancelled(), "GPU operation cancelled");
    cuda_.CheckTarget();
    Reply reply;
    // Allocate the metric message before touching CUDA. Returning measurements
    // requires only numeric assignments, never formatting or JSON serialization.
    auto& metrics = *reply.mutable_metrics();
    if (next == Op::LOCK) {
      cuda_.Lock();
      reply.set_status(Reply::LOCKED);
    } else if (next == Op::PREPARE) {
      Prepare(metrics);
      reply.set_status(Reply::PREPARED);
    } else if (next == Op::TRANSFER) {
      Transfer(metrics);
      reply.set_status(Reply::TRANSFERRED);
    } else {
      Complete(metrics);
      reply.set_status(Reply::COMPLETE);
    }
    phase_ = next;
    return reply;
  }

  void
  Drain()
  {
    if (phase_ != protocol::GPUSessionRequest::COMPLETE)
      cuda_.Abort();
  }

private:
  void
  Prepare(protocol::GPUSessionMetrics& metrics)
  {
    start_ = Clock::now();
    std::string error;
    const auto begin = Clock::now();
    view_ = cuda_.Prepare(save_, gpu_pairs_, custom_storage_);
    const auto end = Clock::now();
    prepare_seconds_ = std::chrono::duration<double>(end - begin).count();
    metrics.set_prepare_seconds(prepare_seconds_);
    metrics.set_prepare_start_ns(Nanoseconds(begin));
    metrics.set_prepare_end_ns(Nanoseconds(end));
    if (!custom_storage_)
      return;
    Require(view_->deviceCount <= engine_.devices.size(), "too many CustomStorage devices");
    std::vector<storage::DeviceExtent> extents;
    for (unsigned index = 0; index < view_->deviceCount; ++index) {
      CUcontext context = nullptr;
      Check(cuStreamGetCtx(view_->perDeviceData[index].stream, &context), "stream context");
      auto& device = engine_.DeviceForContext(context);
      owners_.push_back(&device);
      extents.push_back({device.uuid, view_->perDeviceData[index].size});
    }
    if (save_ && !storage::BuildCheckpointManifest(extents, &manifest_, &error))
      throw std::runtime_error(error);
    if (!storage::BuildTransferJobs(manifest_, extents, device_pairs_, &jobs_, &error))
      throw std::runtime_error(error);
    if (save_ && enable_checksum_digest_)
      extent_digests_.resize(manifest_.size());
  }

  void
  Transfer(protocol::GPUSessionMetrics& metrics)
  {
    if (!custom_storage_)
      return;
    const auto begin = Clock::now();
    for (const auto& job : jobs_) {
      Require(!cancellation_.IsCancelled(), "GPU transfer cancelled");
      const auto& data = view_->perDeviceData[job.device_index];
      const auto path = directory_ / manifest_[job.extent_index].filename;
      FileDescriptor file(
          open(path.c_str(), O_CLOEXEC | O_NOFOLLOW | (save_ ? O_CREAT | O_TRUNC | O_RDWR : O_RDONLY), 0600));
      Require(file.get() >= 0 && (!save_ || !ftruncate(file.get(), data.size)), "open or size GPU extent");
      transfer::TransferMetrics transfer_metrics;
      auto& device = *owners_[job.device_index];
      std::lock_guard lease(device.mutex);
      std::string error;
      if (!device.buffers.Transfer(file.get(), data.devPtr, data.size, data.stream,
                                   save_ ? transfer::TransferOperation::kCheckpoint
                                         : transfer::TransferOperation::kRestore,
                                   &transfer_metrics, &error, {&cancellation_, enable_checksum_digest_}))
        throw std::runtime_error(error);
      if (enable_checksum_digest_) {
        if (save_)
          extent_digests_[job.extent_index] = transfer_metrics.sha256;
        else
          Require(transfer_metrics.sha256 == extent_digests_[job.extent_index], "GPU extent SHA-256 mismatch");
      }
      bytes_ += transfer_metrics.bytes;
      setup_seconds_ += transfer_metrics.setup_seconds;
      storage_seconds_ += transfer_metrics.storage_io_seconds;
      cuda_wait_seconds_ += transfer_metrics.cuda_wait_seconds;
    }
    const auto end = Clock::now();
    transfer_seconds_ = std::chrono::duration<double>(end - begin).count();
    metrics.set_bytes(bytes_);
    metrics.set_transfer_seconds(transfer_seconds_);
    metrics.set_transfer_setup_seconds(setup_seconds_);
    metrics.set_storage_request_service_seconds(storage_seconds_);
    metrics.set_cuda_wait_seconds(cuda_wait_seconds_);
    metrics.set_transfer_start_ns(Nanoseconds(begin));
    metrics.set_transfer_end_ns(Nanoseconds(end));
  }

  void
  Complete(protocol::GPUSessionMetrics& metrics)
  {
    const auto begin = Clock::now();
    cuda_.Complete();
    const double completion = Seconds(begin);
    std::string error;
    double unlock = 0;
    if (save_ && custom_storage_) {
      if (!storage::WriteManifest(directory_, manifest_, &error))
        throw std::runtime_error(error);
      if (enable_checksum_digest_ && !storage::WriteExtentDigests(directory_, manifest_, extent_digests_, &error))
        throw std::runtime_error(error);
    } else if (!save_) {
      Require(!cancellation_.IsCancelled(), "GPU restore cancelled before unlock");
      const auto unlock_start = Clock::now();
      cuda_.Unlock();
      unlock = Seconds(unlock_start);
    }
    metrics.set_bytes(bytes_);
    metrics.set_prepare_seconds(prepare_seconds_);
    metrics.set_transfer_seconds(transfer_seconds_);
    metrics.set_transfer_setup_seconds(setup_seconds_);
    metrics.set_storage_request_service_seconds(storage_seconds_);
    metrics.set_cuda_wait_seconds(cuda_wait_seconds_);
    metrics.set_complete_seconds(completion);
    metrics.set_unlock_seconds(unlock);
    metrics.set_total_seconds(Seconds(start_));
  }

  Engine& engine_;
  FileDescriptor directory_fd_;
  std::filesystem::path directory_;
  bool save_;
  bool custom_storage_;
  bool enable_checksum_digest_;
  transfer::TransferCancellation& cancellation_;
  std::vector<std::string> extent_digests_;
  checkpoint::Operation cuda_;
  protocol::GPUSessionRequest::Operation phase_ = protocol::GPUSessionRequest::UNSPECIFIED;
  const CUcheckpointCustomStorageInfo* view_ = nullptr;
  std::vector<CUcheckpointGpuPair> gpu_pairs_;
  std::vector<storage::DevicePair> device_pairs_;
  std::vector<storage::ManifestExtent> manifest_;
  std::vector<storage::TransferJob> jobs_;
  std::vector<Device*> owners_;
  Clock::time_point start_;
  size_t bytes_ = 0;
  double prepare_seconds_ = 0, transfer_seconds_ = 0, setup_seconds_ = 0, storage_seconds_ = 0, cuda_wait_seconds_ = 0;
};

// Admission owns the pinned namespace and artifact directory before CRIU. CUDA
// target identity is resolved only on the first phase after restored PIDs exist.
class Session {
public:
  Session(Engine& engine, protocol::BindGPUSession binding, FileDescriptor root)
      : engine_(engine), binding_(std::move(binding))
  {
    Require(binding_.namespace_pid() > 0 && binding_.namespace_pid() <= INT_MAX && binding_.visible_devices_size() > 0,
            "GPU binding requires target identity and GPUs");
    Require(binding_.direction() == protocol::BindGPUSession::SAVE ||
                binding_.direction() == protocol::BindGPUSession::LOAD,
            "invalid GPU direction");
    const bool custom = binding_.storage_mode() == protocol::BindGPUSession::CUSTOM_STORAGE;
    Require(custom || binding_.storage_mode() == protocol::BindGPUSession::DRIVER_MANAGED, "unknown GPU storage mode");
    Require(custom || !binding_.enable_checksum_digest(),
            "checksum digests are only supported for CustomStorage extents");
    if (custom)
      engine_.checkpoint.RequireCustomStorage();
    for (const auto& uuid : binding_.visible_devices()) {
      Require(std::any_of(engine_.devices.begin(), engine_.devices.end(),
                          [&](const auto& device) { return device.second->uuid == uuid; }),
              "target GPU is not available in the persistent helper");
    }
    namespace_ = engine_.PinNamespace(binding_.container_pid());
    struct stat info{};
    Require(!fstat(root.get(), &info) && S_ISDIR(info.st_mode), "GPU artifact root must be a directory");
    if (custom) {
      const bool save = binding_.direction() == protocol::BindGPUSession::SAVE;
      // Keep the existing native/<captured namespace PID> artifact layout.
      if (save)
        Require(mkdirat(root.get(), "native", 0700) == 0 || errno == EEXIST, "create GPU root");
      FileDescriptor extents(openat(root.get(), "native", O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW));
      Require(extents.get() >= 0, "open GPU root");
      const auto name = std::to_string(binding_.namespace_pid());
      if (save)
        Require(mkdirat(extents.get(), name.c_str(), 0700) == 0, "create GPU target directory");
      directory_ = FileDescriptor(openat(extents.get(), name.c_str(), O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW));
      Require(directory_.get() >= 0, "pin GPU target directory");
      if (!save) {
        const std::filesystem::path path("/proc/self/fd/" + std::to_string(directory_.get()));
        std::string error;
        // Read and stat before CRIU and before any participant begins transfer.
        if (!storage::ReadManifest(path, &manifest_, &error) ||
            !storage::ValidateExtentFiles(path, manifest_, &error) ||
            (binding_.enable_checksum_digest() && !storage::ReadExtentDigests(path, manifest_, &digests_, &error)))
          throw std::runtime_error(error);
      }
    } else {
      directory_ = std::move(root);
    }
    Require(!fstat(directory_.get(), &info) && !(info.st_mode & 0022), "GPU storage must be a private directory");
  }
  protocol::GPUSessionReply
  Execute(const protocol::GPUSessionRequest& request)
  {
    Require(!cancellation_.IsCancelled(), "GPU session cancelled");
    if (!operation_) {
      const auto target_pid = request.target_pid() ? request.target_pid() : binding_.namespace_pid();
      const auto host_pid = namespace_->Resolve(target_pid);
      auto operation = std::make_unique<Operation>(engine_, binding_, host_pid, std::move(directory_), cancellation_,
                                                   std::move(manifest_), std::move(digests_));
      // The operation has opened a pidfd; confirm namespace identity again
      // before it can perform any CUDA action on that pinned process.
      Require(namespace_->Matches(host_pid, target_pid), "GPU target changed during admission");
      operation_ = std::move(operation);
    }
    return operation_->Execute(request);
  }
  void
  Cancel()
  {
    cancellation_.Cancel();
  }
  void
  Drain()
  {
    if (operation_)
      operation_->Drain();
    operation_.reset();
  }

private:
  Engine& engine_;
  protocol::BindGPUSession binding_;
  std::shared_ptr<PidNamespace> namespace_;
  FileDescriptor directory_{-1};
  std::vector<storage::ManifestExtent> manifest_;
  std::vector<std::string> digests_;
  transfer::TransferCancellation cancellation_;
  std::unique_ptr<Operation> operation_;
};

void
ServeSession(Engine& engine, protocol::BindGPUSession binding, FileDescriptor connection, FileDescriptor directory)
{
  std::unique_ptr<Session> session;
  protocol::GPUSessionReply reply;
  try {
    session = std::make_unique<Session>(engine, binding, std::move(directory));
    reply.set_status(protocol::GPUSessionReply::READY);
  } catch (const std::exception& error) {
    reply.mutable_failure()->set_code(protocol::Failure::INTERNAL_ERROR);
    reply.mutable_failure()->set_message(error.what());
  }
  try {
    SendFrame(connection.get(), reply);
    if (session) {
      // The agent half-closes abandoned sessions. Observe this while Execute is
      // busy, without a second thread consuming phase commands.
      FileDescriptor cancellation_wakeup(eventfd(0, EFD_CLOEXEC | EFD_NONBLOCK));
      Require(cancellation_wakeup.get() >= 0, "create cancellation wakeup");
      std::jthread cancellation([&](std::stop_token stop) {
        std::stop_callback wake(stop, [&] {
          const uint64_t value = 1;
          const auto ignored = write(cancellation_wakeup.get(), &value, sizeof(value));
          (void)ignored;
        });
        pollfd descriptors[]{{connection.get(), POLLRDHUP, 0}, {cancellation_wakeup.get(), POLLIN, 0}};
        while (!stop.stop_requested()) {
          const int count = poll(descriptors, 2, -1);
          if (count > 0 && (descriptors[0].revents & (POLLRDHUP | POLLHUP | POLLERR))) {
            session->Cancel();
            return;
          }
        }
      });
      protocol::GPUSessionRequest request;
      std::vector<FileDescriptor> descriptors;
      while (ReceiveFrame(connection.get(), request, descriptors)) {
        Require(descriptors.empty(), "GPU helper command cannot carry descriptors");
        reply.Clear();
        try {
          reply = session->Execute(request);
        } catch (const checkpoint::CompletionUncertain& error) {
          reply.mutable_failure()->set_code(protocol::Failure::INTERNAL_ERROR);
          reply.mutable_failure()->set_message(error.what());
          SendFrame(connection.get(), reply);
          break; // Drain kills the affected target and retires the helper.
        } catch (const std::exception& error) {
          reply.mutable_failure()->set_code(protocol::Failure::INTERNAL_ERROR);
          reply.mutable_failure()->set_message(error.what());
        }
        SendFrame(connection.get(), reply);
      }
    }
  } catch (const std::exception& error) {
    std::osyncstream(std::cerr) << "CUDA session disconnected namespace_pid=" << binding.namespace_pid()
                                << " error=" << error.what() << '\n';
  }
  // The observer is joined before dropping session state. Drain failure is
  // process-fatal: the parent must reap this helper before releasing artifacts.
  try {
    if (session)
      session->Drain();
    session.reset();
  } catch (const std::exception& error) {
    std::osyncstream(std::cerr) << "CUDA helper drain failed: " << error.what() << '\n';
    std::_Exit(1);
  }
  reply.Clear();
  reply.set_status(protocol::GPUSessionReply::DRAINED);
  try {
    SendFrame(connection.get(), reply);
  } catch (const std::exception&) {
  }
}
} // namespace

extern "C" int cuda_checkpoint_cli_main(int argc, char** argv);

int
main(int argc, char** argv)
{
  if (argc < 2 || std::string_view(argv[1]) != "--daemon")
    return cuda_checkpoint_cli_main(argc, argv);
  try {
    WatchOwner(3);
    Engine engine(ParseOptions(argc - 1, argv + 1));
    protocol::GPUSessionReply ready;
    ready.set_status(protocol::GPUSessionReply::READY);
    ready.set_custom_storage_available(engine.checkpoint.SupportsCustomStorage());
    *ready.mutable_metrics() = engine.ready_metrics;
    SendFrame(3, ready);
    std::map<uint64_t, std::future<void>> sessions;
    protocol::HelperRequest request;
    std::vector<FileDescriptor> descriptors;
    try {
      while (ReceiveFrame(3, request, descriptors)) {
        const auto id = request.session_id();
        Require(id != 0, "CUDA admission requires a session ID");
        if (request.has_bind()) {
          Require(descriptors.size() == 2 && !sessions.contains(id), "invalid CUDA helper admission");
          sessions.emplace(id, std::async(std::launch::async, ServeSession, std::ref(engine), request.bind(),
                                          std::move(descriptors[0]), std::move(descriptors[1])));
          descriptors.clear();
        } else {
          Require(request.has_drain() && request.drain() && descriptors.empty(), "invalid CUDA helper drain");
          // The parent already half-closed every sibling session. Joining here
          // waits for cancellation, DMA/I/O drain and descriptor release without
          // racing nsrestore's reader on the session endpoint.
          if (const auto session = sessions.find(id); session != sessions.end()) {
            session->second.get();
            sessions.erase(session);
          }
          protocol::GPUSessionReply drained;
          drained.set_status(protocol::GPUSessionReply::DRAINED);
          drained.set_session_id(id);
          SendFrame(3, drained);
        }
      }
    } catch (const std::exception& error) {
      // Exit before destroying outstanding async futures: their sessions may
      // still be waiting for commands after the owner loses the control socket.
      std::osyncstream(std::cerr) << "CUDA helper control failed: " << error.what() << '\n';
      std::_Exit(1);
    }
    // Process exit is the context lifetime boundary; no per-session teardown
    // releases primary contexts still used by restored processes.
    std::_Exit(0);
  } catch (const std::exception& error) {
    std::osyncstream(std::cerr) << "CUDA helper failed: " << error.what() << '\n';
    std::_Exit(1);
  }
}

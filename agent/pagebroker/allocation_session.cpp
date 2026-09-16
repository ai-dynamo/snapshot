// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "allocation_session.hpp"

#include <fcntl.h>
#include <signal.h>
#include <spawn.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>
#include <algorithm>
#include <cerrno>
#include <fstream>
#include <limits>
#include <stdexcept>
#include <system_error>

extern char** environ;

namespace snapshot::pagebroker {
namespace {
void Require(bool condition, const char* message)
{
  if (!condition)
    throw std::runtime_error(message);
}

bool Hex(const std::string& value, size_t size)
{
  return value.size() == size && std::all_of(value.begin(), value.end(), [](char c) {
    return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f');
  });
}

void Check(int status, const char* message)
{
  if (status < 0)
    throw std::system_error(errno, std::generic_category(), message);
}

void ValidateExtent(const v1::AllocationExtent& extent)
{
  Require(Hex(extent.allocation_id(), 32) && extent.device_uuid().size() == 16 &&
          extent.size() && extent.size() <= static_cast<uint64_t>(std::numeric_limits<off_t>::max()),
          "invalid allocation identity, device or size");
}

void WriteManifest(int directory, const v1::AllocationManifest& manifest)
{
  FileDescriptor output(openat(directory, "manifest.tmp", O_WRONLY | O_CREAT | O_EXCL | O_CLOEXEC | O_NOFOLLOW, 0600));
  Check(output.get(), "create allocation manifest");
  const std::string bytes = manifest.SerializeAsString();
  for (size_t offset = 0; offset < bytes.size();) {
    ssize_t count = write(output.get(), bytes.data() + offset, bytes.size() - offset);
    if (count < 0 && errno == EINTR)
      continue;
    Check(count, "write allocation manifest");
    Require(count != 0, "allocation manifest write made no progress");
    offset += count;
  }
  Check(fsync(output.get()), "sync allocation manifest");
  Check(renameat(directory, "manifest.tmp", directory, "manifest.pb"), "publish allocation manifest");
  Check(fsync(directory), "sync allocation directory");
}
}  // namespace

class AllocationSession::Worker {
 public:
  explicit Worker(const Path& executable)
  {
    Require(!executable.empty() && executable.is_absolute(), "allocation worker is not configured");
    int sockets[2];
    Check(socketpair(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0, sockets), "worker socketpair");
    connection_ = FileDescriptor(sockets[0]);
    FileDescriptor child(sockets[1]);
    SetAllocationTimeout(connection_.get());
    posix_spawn_file_actions_t actions;
    int error = posix_spawn_file_actions_init(&actions);
    if (error)
      throw std::system_error(error, std::generic_category(), "worker spawn actions");
    // Reserve descriptor 3, then close every unrelated inherited descriptor.
    error = posix_spawn_file_actions_adddup2(&actions, child.get(), 3);
    if (!error)
      error = posix_spawn_file_actions_addclosefrom_np(&actions, 4);
    std::string binary = executable.string();
    char argument[] = "--socket-fd=3";
    char* argv[] = {binary.data(), argument, nullptr};
    if (!error)
      error = posix_spawn(&pid_, binary.c_str(), &actions, nullptr, argv, environ);
    posix_spawn_file_actions_destroy(&actions);
    if (error)
      throw std::system_error(error, std::generic_category(), "spawn allocation worker");
    try {
      v1::AllocationSessionReply ready;
      std::vector<FileDescriptor> received;
      Require(ReceiveFrame(connection_.get(), ready, received) && received.empty() &&
              ready.has_completed() && ready.completed().extents().empty(), "allocation worker unavailable");
    } catch (...) {
      Stop();
      throw;
    }
  }
  ~Worker() { Stop(); }

  v1::AllocationSessionReply Transfer(const v1::AllocationWorkerRequest& request, const std::vector<int>& descriptors)
  {
    SendFrame(connection_.get(), request, descriptors);
    v1::AllocationSessionReply reply;
    std::vector<FileDescriptor> received;
    Require(ReceiveFrame(connection_.get(), reply, received) && received.empty(), "allocation worker disconnected");
    Require(reply.has_completed(), reply.has_failure() ? reply.failure().message().c_str() : "invalid worker response");
    return reply;
  }

 private:
  void Stop() noexcept
  {
    if (pid_ <= 0)
      return;
    // A failed/timed-out worker cannot leave a live DMA reference when session
    // admission is released. Reap it before transaction cleanup is possible.
    kill(pid_, SIGKILL);
    while (waitpid(pid_, nullptr, 0) < 0 && errno == EINTR) {}
    pid_ = -1;
  }
  FileDescriptor connection_{-1};
  pid_t pid_ = -1;
};

AllocationSession::AllocationSession(std::shared_ptr<Transaction> transaction,
                                     const v1::BindAllocationSession& binding, const Path& worker)
    : transaction_(std::move(transaction)), binding_(binding)
{
  Require(Hex(binding.participant_id(), 32), "invalid participant identity");
  Require(binding.direction() == v1::BindAllocationSession::SAVE ||
          binding.direction() == v1::BindAllocationSession::LOAD, "invalid allocation direction");
  // Probe a real worker before granting a capability to mutate allocations.
  worker_ = std::make_unique<Worker>(worker);
  std::lock_guard lock(transaction_->mutex());
  Require(transaction_->state() == Transaction::State::STAGED && !transaction_->allocation_failed,
          "allocation transaction is not staged");
  Require(!transaction_->allocation_participants.contains(binding.participant_id()), "participant already bound");
  Path staging;
  FileDescriptor root(-1);
  if (binding.direction() == v1::BindAllocationSession::SAVE) {
    const auto* descriptor = std::get_if<CheckpointTransactionDescriptor>(&transaction_->descriptor());
    Require(descriptor != nullptr, "save requires checkpoint transaction");
    staging = descriptor->staging_directory();
  } else {
    if (const auto* direct = std::get_if<DirectRestoreDescriptor>(&transaction_->descriptor())) {
      root = FileDescriptor(fcntl(direct->source_directory.get(), F_DUPFD_CLOEXEC, 0));
      Check(root.get(), "retain direct allocation source");
    } else {
      const auto* descriptor = std::get_if<RestoreTransactionDescriptor>(&transaction_->descriptor());
      Require(descriptor != nullptr, "load requires restore transaction");
      staging = descriptor->staging_directory();
    }
  }
  // A failed bind after creating filesystem state must also prevent Commit.
  // This mutex excludes other admissions until setup has either succeeded or
  // left this sticky failure behind. Abort remains available.
  transaction_->allocation_failed = true;
  if (root.get() < 0)
    root = FileDescriptor(open(staging.c_str(), O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW));
  Check(root.get(), "open allocation staging root");
  if (binding.direction() == v1::BindAllocationSession::SAVE) {
    if (mkdirat(root.get(), "allocations", 0700) < 0 && errno != EEXIST)
      Check(-1, "create allocations directory");
  }
  FileDescriptor allocations(openat(root.get(), "allocations", O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW));
  Check(allocations.get(), "open allocations directory");
  if (binding.direction() == v1::BindAllocationSession::SAVE) {
    Check(mkdirat(allocations.get(), binding.participant_id().c_str(), 0700), "create participant directory");
    Check(fsync(allocations.get()), "sync participant directory entry");
    Check(fsync(root.get()), "sync allocations directory entry");
  }
  directory_fd_ = FileDescriptor(openat(allocations.get(), binding.participant_id().c_str(),
                                       O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW));
  Check(directory_fd_.get(), "open participant directory");
  content_fd_ = FileDescriptor(openat(directory_fd_.get(), "content.bin",
      (binding.direction() == v1::BindAllocationSession::SAVE ? O_RDWR | O_CREAT | O_EXCL : O_RDONLY | O_NONBLOCK) |
          O_CLOEXEC | O_NOFOLLOW, 0600));
  Check(content_fd_.get(), "open participant content");
  if (binding.direction() == v1::BindAllocationSession::LOAD) {
    FileDescriptor manifest_fd(openat(directory_fd_.get(), "manifest.pb", O_RDONLY | O_NONBLOCK | O_CLOEXEC | O_NOFOLLOW));
    Check(manifest_fd.get(), "open allocation manifest");
    struct stat stat{};
    Check(fstat(manifest_fd.get(), &stat), "stat allocation manifest");
    Require(S_ISREG(stat.st_mode) && stat.st_size >= 0 && stat.st_size <= (16 << 20), "invalid allocation manifest size");
    std::string bytes(stat.st_size, '\0');
    for (size_t offset = 0; offset < bytes.size();) {
      const ssize_t count = pread(manifest_fd.get(), bytes.data() + offset, bytes.size() - offset, offset);
      if (count < 0 && errno == EINTR)
        continue;
      Check(count, "read allocation manifest");
      Require(count != 0, "short allocation manifest");
      offset += count;
    }
    v1::AllocationManifest manifest;
    Require(manifest.ParseFromString(bytes) && manifest.version() == 2 &&
            manifest.storage_offsets_size() == manifest.extents_size() &&
            manifest.participant_id() == binding.participant_id(), "invalid allocation manifest");
    std::vector<std::pair<uint64_t, uint64_t>> ranges;
    for (int index = 0; index < manifest.extents_size(); ++index) {
      const auto& extent = manifest.extents(index);
      ValidateExtent(extent);
      Require(extents_.emplace(extent.allocation_id(), extent).second, "duplicate allocation");
      const uint64_t offset = manifest.storage_offsets(index);
      Require(offset <= static_cast<uint64_t>(std::numeric_limits<off_t>::max()) - extent.size(),
              "allocation content offset overflow");
      offsets_.emplace(extent.allocation_id(), offset);
      ranges.emplace_back(offset, offset + extent.size());
    }
    std::sort(ranges.begin(), ranges.end());
    for (const auto& [begin, end] : ranges) {
      Require(begin == content_size_, "allocation content ranges overlap or leave gaps");
      content_size_ = end;
    }
    struct stat content_stat{};
    Check(fstat(content_fd_.get(), &content_stat), "stat participant content");
    Require(S_ISREG(content_stat.st_mode) && content_stat.st_size >= 0 &&
            static_cast<uint64_t>(content_stat.st_size) == content_size_, "participant content size mismatch");
  }
  transaction_->allocation_participants.insert(binding.participant_id());
  ++transaction_->allocation_sessions;
  transaction_->allocation_failed = false;
  admitted_ = true;
}

AllocationSession::~AllocationSession()
{
  worker_.reset();
  if (admitted_) {
    std::lock_guard lock(transaction_->mutex());
    --transaction_->allocation_sessions;
    transaction_->allocation_failed |= !finished_;
  }
}

v1::AllocationSessionReply AllocationSession::Execute(const v1::AllocationSessionRequest& request,
                                                     const std::vector<FileDescriptor>& descriptors)
{
  v1::AllocationSessionReply reply;
  try {
    Require(!finished_ && !failed_, "allocation session is terminal");
    if (request.has_finish()) {
      Require(descriptors.empty(), "finish cannot carry descriptors");
      if (binding_.direction() == v1::BindAllocationSession::SAVE) {
        Check(fsync(content_fd_.get()), "sync participant content");
        v1::AllocationManifest manifest;
        manifest.set_version(2);
        manifest.set_participant_id(binding_.participant_id());
        for (const auto& [id, extent] : extents_) {
          *manifest.add_extents() = extent;
          manifest.add_storage_offsets(offsets_.at(id));
        }
        Require(manifest.ByteSizeLong() <= (16 << 20), "allocation manifest exceeds limit");
        WriteManifest(directory_fd_.get(), manifest);
      } else {
        Require(transferred_.size() == extents_.size(), "load did not cover every saved allocation");
      }
      worker_.reset();
      finished_ = true;
      {
        std::lock_guard lock(transaction_->mutex());
        --transaction_->allocation_sessions;
        admitted_ = false;
      }
      reply.mutable_finished();
      return reply;
    }
    Require(request.has_batch() && request.batch().extents_size() > 0 &&
            request.batch().extents_size() <= static_cast<int>(kAllocationBatchLimit) &&
            descriptors.size() == static_cast<size_t>(request.batch().extents_size()), "invalid allocation batch");
    v1::AllocationWorkerRequest work;
    work.set_direction(binding_.direction());
    std::vector<int> rights;
    for (const auto& descriptor : descriptors)
      rights.push_back(descriptor.get());
    for (const auto& extent : request.batch().extents()) {
      ValidateExtent(extent);
      Require(transferred_.insert(extent.allocation_id()).second, "duplicate allocation");
      // Keep session metadata bounded as well as individual wire frames.
      Require(transferred_.size() <= 65536, "allocation session exceeds extent limit");
      *work.mutable_batch()->add_extents() = extent;
      const bool save = binding_.direction() == v1::BindAllocationSession::SAVE;
      if (!save) {
        const auto found = extents_.find(extent.allocation_id());
        Require(found != extents_.end() && found->second.size() == extent.size(), "allocation absent from saved manifest");
      } else {
        Require(content_size_ <= static_cast<uint64_t>(std::numeric_limits<off_t>::max()) - extent.size(),
                "participant content size overflow");
        offsets_.emplace(extent.allocation_id(), content_size_);
        content_size_ += extent.size();
      }
      work.add_storage_offsets(offsets_.at(extent.allocation_id()));
    }
    if (binding_.direction() == v1::BindAllocationSession::SAVE)
      Check(ftruncate(content_fd_.get(), content_size_), "size participant content");
    rights.push_back(content_fd_.get());
    reply = worker_->Transfer(work, rights);
    Require(reply.completed().extents_size() == work.batch().extents_size(), "worker returned incomplete batch");
    for (int i = 0; i < reply.completed().extents_size(); ++i) {
      const auto& extent = reply.completed().extents(i);
      const auto& expected = work.batch().extents(i);
      Require(extent.allocation_id() == expected.allocation_id() && extent.size() == expected.size() &&
              extent.device_uuid() == expected.device_uuid(), "worker extent mismatch");
      if (binding_.direction() == v1::BindAllocationSession::SAVE)
        extents_.emplace(extent.allocation_id(), extent);
    }
  } catch (const std::exception& error) {
    // Stop/reap before returning failure. No automatic retry can reuse a
    // partially loaded allocation or publish an incomplete save.
    worker_.reset();
    failed_ = true;
    reply.Clear();
    reply.mutable_failure()->set_code(Failure::STORAGE_ERROR);
    reply.mutable_failure()->set_message(error.what());
  }
  return reply;
}
}  // namespace snapshot::pagebroker

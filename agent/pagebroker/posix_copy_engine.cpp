// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "posix_copy_engine.hpp"

#include "../cmd/cuda-checkpoint-helper/content_digest.h"
#include "file_descriptor.hpp"
#include "../cmd/cuda-checkpoint-helper/storage_manifest.h"

#include <array>
#include <atomic>
#include <cerrno>
#include <dirent.h>
#include <exception>
#include <filesystem>
#include <fcntl.h>
#include <linux/openat2.h>
#include <limits>
#include <memory>
#include <optional>
#include <semaphore>
#include <stdexcept>
#include <string_view>
#include <system_error>
#include <sys/random.h>
#include <sys/file.h>
#include <sys/syscall.h>
#include <sys/stat.h>
#include <thread>
#include <unistd.h>
#include <utility>
#include <unordered_set>
#include <vector>

namespace snapshot::pagebroker {
namespace storage = cuda_checkpoint_storage;
namespace {
Path StorageRelativePath(const StorageBackend& storage,
                         const Path& storage_root,
                         const char* label)
{
  if (!storage.has_filesystem() || storage.filesystem().directory().empty())
    throw std::invalid_argument(std::string("filesystem ") + label + " is required");
  const Path path(storage.filesystem().directory());
  const Path relative = path.lexically_relative(storage_root);
  if (!path.is_absolute() || path.lexically_normal() != path || relative.empty() ||
      relative == "." || relative.string().starts_with("../") || relative == "..")
    throw std::invalid_argument(std::string(label) + " must be within storage root");

  return relative;
}

FileDescriptor OpenStorageDirectory(int storage_root_fd,
                                    const Path& relative,
                                    const Path& storage_root,
                                    const char* label)
{
  open_how how{};
  how.flags = O_RDONLY | O_DIRECTORY | O_CLOEXEC;
  how.resolve = RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS |
                RESOLVE_NO_MAGICLINKS;
  const std::string value = relative.string();
  const int fd = static_cast<int>(
      syscall(SYS_openat2, storage_root_fd, value.c_str(), &how, sizeof(how)));
  if (fd >= 0)
    return FileDescriptor(fd);
  if (errno != ENOSYS) {
    throw std::filesystem::filesystem_error(
        std::string("cannot open storage ") + label,
        storage_root / relative,
        std::error_code(errno, std::generic_category()));
  }

  // Docker seccomp profiles and older kernels may report openat2 as ENOSYS.
  // Component-wise openat with O_NOFOLLOW preserves the same pinned-root,
  // no-symlink property without falling back to pathname validation.
  const int duplicate = fcntl(storage_root_fd, F_DUPFD_CLOEXEC, 0);
  if (duplicate < 0)
    throw std::filesystem::filesystem_error(
        "cannot duplicate storage root", storage_root,
        std::error_code(errno, std::generic_category()));
  FileDescriptor current(duplicate);
  Path walked;
  for (const auto& part : relative) {
    walked /= part;
    const std::string component = part.string();
    const int next = openat(current.get(), component.c_str(),
                            O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);
    if (next < 0)
      throw std::filesystem::filesystem_error(
          std::string("cannot open storage ") + label,
          storage_root / walked,
          std::error_code(errno, std::generic_category()));
    current = FileDescriptor(next);
  }
  return current;
}

FileDescriptor OpenStorageParent(int storage_root_fd,
                                 const Path& relative,
                                 const Path& storage_root,
                                 bool create)
{
  const int duplicate = fcntl(storage_root_fd, F_DUPFD_CLOEXEC, 0);
  if (duplicate < 0)
    throw std::filesystem::filesystem_error(
        "cannot duplicate storage root", storage_root,
        std::error_code(errno, std::generic_category()));
  FileDescriptor current(duplicate);
  Path walked;
  for (const auto& part : relative.parent_path()) {
    walked /= part;
    const std::string component = part.string();
    int next = openat(current.get(), component.c_str(),
                      O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);
    if (next < 0 && errno == ENOENT && create) {
      if (mkdirat(current.get(), component.c_str(), 0700) != 0 &&
          errno != EEXIST)
        throw std::filesystem::filesystem_error(
            "cannot create storage destination parent", storage_root / walked,
            std::error_code(errno, std::generic_category()));
      next = openat(current.get(), component.c_str(),
                    O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);
    }
    if (next < 0) {
      if (errno == ENOENT && !create)
        return FileDescriptor(-1);
      throw std::filesystem::filesystem_error(
          "cannot open storage destination parent", storage_root / walked,
          std::error_code(errno, std::generic_category()));
    }
    current = FileDescriptor(next);
  }
  return current;
}

std::string PartialPrefix(const Path& destination)
{
  return destination.filename().string() + ".pagebroker-partial.";
}

std::string PreviousLeaf(const Path& destination)
{
  return destination.filename().string() + ".pagebroker-previous";
}

std::string RandomPartialLeaf(const Path& destination)
{
  std::array<unsigned char, 16> random{};
  size_t offset = 0;
  while (offset < random.size()) {
    const ssize_t count = getrandom(random.data() + offset,
                                    random.size() - offset, 0);
    if (count < 0 && errno == EINTR)
      continue;
    if (count <= 0)
      throw std::system_error(
          errno == 0 ? EIO : errno, std::generic_category(),
          "cannot generate checkpoint temporary name");
    offset += static_cast<size_t>(count);
  }
  constexpr char hex[] = "0123456789abcdef";
  std::string leaf = PartialPrefix(destination);
  leaf.reserve(leaf.size() + random.size() * 2);
  for (const unsigned char value : random) {
    leaf.push_back(hex[value >> 4]);
    leaf.push_back(hex[value & 0x0f]);
  }
  return leaf;
}

using Directory = std::unique_ptr<DIR, int (*)(DIR*)>;

[[noreturn]] void ThrowCopyError(const char* operation,
                                 const Path& source,
                                 const Path& destination,
                                 int error);

Directory OpenDirectoryStream(int fd, const Path& path)
{
  // dup() shares a directory stream offset with the pinned descriptor. A
  // direct restore deliberately scans the same source generation once for
  // admission/accounting and again for copy, so give each scan its own open
  // file description while staying beneath the already-open directory.
  const int duplicate = openat(
      fd, ".", O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);
  if (duplicate < 0)
    throw std::filesystem::filesystem_error(
        "cannot duplicate source directory", path,
        std::error_code(errno, std::generic_category()));
  DIR* directory = fdopendir(duplicate);
  if (directory == nullptr) {
    const int error = errno;
    close(duplicate);
    throw std::filesystem::filesystem_error(
        "cannot read source directory", path,
        std::error_code(error, std::generic_category()));
  }
  return Directory(directory, closedir);
}

uintmax_t DirectorySizeAt(int directory_fd, const Path& display_path)
{
  uintmax_t bytes = 0;
  auto directory = OpenDirectoryStream(directory_fd, display_path);
  for (;;) {
    errno = 0;
    const dirent* entry = readdir(directory.get());
    if (entry == nullptr) {
      if (errno != 0)
        ThrowCopyError("cannot read source directory", display_path, {}, errno);
      break;
    }
    const std::string name(entry->d_name);
    if (name == "." || name == "..")
      continue;
    struct stat status{};
    if (fstatat(directory_fd, name.c_str(), &status,
                AT_SYMLINK_NOFOLLOW) != 0)
      ThrowCopyError("cannot stat source entry", display_path / name, {},
                     errno);
    if (S_ISLNK(status.st_mode))
      throw std::runtime_error("checkpoint contains symlink");
    if (S_ISDIR(status.st_mode)) {
      const int child = openat(directory_fd, name.c_str(),
                               O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);
      if (child < 0)
        ThrowCopyError("cannot open source directory", display_path / name,
                       {}, errno);
      const FileDescriptor child_fd(child);
      const uintmax_t child_bytes =
          DirectorySizeAt(child_fd.get(), display_path / name);
      if (child_bytes > std::numeric_limits<uintmax_t>::max() - bytes)
        throw std::overflow_error("directory size overflow");
      bytes += child_bytes;
      continue;
    }
    if (S_ISREG(status.st_mode)) {
      const uintmax_t file_bytes = static_cast<uintmax_t>(status.st_size);
      if (file_bytes > std::numeric_limits<uintmax_t>::max() - bytes)
        throw std::overflow_error("directory size overflow");
      bytes += file_bytes;
      continue;
    }
    throw std::runtime_error("checkpoint contains non-regular file");
  }
  return bytes;
}

struct DirectCopyAccounting {
  uintmax_t copied_bytes = 0;
  uintmax_t retained_bytes = 0;
};

const DirectRestoreProcess* FindDirectProcess(
    const Path& relative, const DirectRestoreProcesses& processes)
{
  for (const auto& process : processes) {
    if (relative == Path("cuda-custom-storage") / process.directory_name)
      return &process;
  }
  return nullptr;
}

const DirectRestoreCarrier* FindDirectCarrier(
    const Path& relative, const DirectRestoreProcesses& processes)
{
  for (const auto& process : processes) {
    const Path directory = Path("cuda-custom-storage") / process.directory_name;
    for (const auto& carrier : process.carriers) {
      if (relative == directory / carrier.filename)
        return &carrier;
    }
  }
  return nullptr;
}

const DirectRestoreProcess* FindDirectManifest(
    const Path& relative, const DirectRestoreProcesses& processes)
{
  for (const auto& process : processes) {
    if (relative == Path("cuda-custom-storage") / process.directory_name /
                        storage::kManifestName)
      return &process;
  }
  return nullptr;
}

DirectCopyAccounting DirectDirectorySizeAt(
    int directory_fd,
    const Path& display_path,
    const Path& relative,
    const DirectRestoreProcesses& processes)
{
  DirectCopyAccounting result;
  auto directory = OpenDirectoryStream(directory_fd, display_path);
  for (;;) {
    errno = 0;
    const dirent* entry = readdir(directory.get());
    if (entry == nullptr) {
      if (errno != 0)
        ThrowCopyError("cannot read source directory", display_path, {}, errno);
      break;
    }
    const std::string name(entry->d_name);
    if (name == "." || name == "..")
      continue;
    const Path child_relative = relative / name;
    const auto* carrier = FindDirectCarrier(child_relative, processes);
    const auto* manifest = FindDirectManifest(child_relative, processes);
    struct stat status{};
    const int stat_status = carrier != nullptr
                                ? fstat(carrier->descriptor.get(), &status)
                            : manifest != nullptr
                                ? fstat(manifest->manifest.get(), &status)
                                : fstatat(directory_fd, name.c_str(), &status,
                                          AT_SYMLINK_NOFOLLOW);
    if (stat_status != 0)
      ThrowCopyError("cannot stat source entry", display_path / name, {}, errno);
    if (S_ISLNK(status.st_mode))
      throw std::runtime_error("checkpoint contains symlink");
    if (S_ISDIR(status.st_mode)) {
      const auto* pinned = FindDirectProcess(child_relative, processes);
      FileDescriptor child_fd(-1);
      const int child = pinned == nullptr
                            ? openat(directory_fd, name.c_str(),
                                     O_RDONLY | O_DIRECTORY | O_CLOEXEC |
                                         O_NOFOLLOW)
                            : pinned->directory.get();
      if (child < 0)
        ThrowCopyError("cannot open source directory", display_path / name, {}, errno);
      if (pinned == nullptr) {
        child_fd = FileDescriptor(child);
        struct stat opened{};
        if (fstat(child_fd.get(), &opened) != 0)
          ThrowCopyError("source directory changed during admission",
                         display_path / name, {}, errno);
        if (!S_ISDIR(opened.st_mode) || opened.st_dev != status.st_dev ||
            opened.st_ino != status.st_ino)
          ThrowCopyError("source directory changed during admission",
                         display_path / name, {}, ESTALE);
      }
      const auto child_result = DirectDirectorySizeAt(
          child, display_path / name, child_relative, processes);
      if (child_result.copied_bytes > std::numeric_limits<uintmax_t>::max() - result.copied_bytes ||
          child_result.retained_bytes > std::numeric_limits<uintmax_t>::max() - result.retained_bytes)
        throw std::overflow_error("directory size overflow");
      result.copied_bytes += child_result.copied_bytes;
      result.retained_bytes += child_result.retained_bytes;
      continue;
    }
    if (!S_ISREG(status.st_mode))
      throw std::runtime_error("checkpoint contains non-regular file");
    const uintmax_t bytes = carrier == nullptr
                                ? static_cast<uintmax_t>(status.st_size)
                                : carrier->size;
    uintmax_t& total = carrier == nullptr ? result.copied_bytes
                                          : result.retained_bytes;
    if (bytes > std::numeric_limits<uintmax_t>::max() - total)
      throw std::overflow_error("directory size overflow");
    total += bytes;
  }
  return result;
}

[[noreturn]] void
ThrowCopyError(const char* operation, const Path& source, const Path& destination, int error)
{
  throw std::filesystem::filesystem_error(
      operation, source, destination, std::error_code(error == 0 ? EIO : error, std::generic_category()));
}

uintmax_t CopyRegularFileAt(int source_directory,
                           const std::string& source_name,
                           const Path& source,
                           int destination_directory,
                           const std::string& destination_name,
                           const Path& destination,
                           uintmax_t* remaining_bytes,
                           int pinned_source_fd = -1,
                           const struct stat* expected_source = nullptr,
                           const std::atomic_bool* stop_requested = nullptr)
{
  FileDescriptor source_owner(-1);
  int source_value = pinned_source_fd;
  if (source_value < 0) {
    source_value = openat(source_directory, source_name.c_str(),
                          O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
    if (source_value < 0)
      ThrowCopyError("cannot open source file", source, destination, errno);
    source_owner = FileDescriptor(source_value);
  }

  struct stat source_stat {};
  if (fstat(source_value, &source_stat) != 0)
    ThrowCopyError("cannot stat source file", source, destination, errno);
  if (!S_ISREG(source_stat.st_mode))
    throw std::runtime_error("checkpoint contains non-regular file");
  if (source_stat.st_size < 0 ||
      (expected_source != nullptr &&
       (!S_ISREG(expected_source->st_mode) ||
        source_stat.st_dev != expected_source->st_dev ||
        source_stat.st_ino != expected_source->st_ino ||
        source_stat.st_size != expected_source->st_size)))
    ThrowCopyError("source file changed while staging", source, destination,
                   ESTALE);

  const int destination_value = openat(
      destination_directory, destination_name.c_str(),
      O_WRONLY | O_CREAT | O_EXCL | O_CLOEXEC | O_NOFOLLOW,
      source_stat.st_mode & 0777);
  if (destination_value < 0)
    ThrowCopyError("cannot create destination file", source, destination, errno);
  FileDescriptor destination_fd(destination_value);

  std::vector<char> buffer(4U * 1024U * 1024U);
  uintmax_t copied_bytes = 0;
  off_t source_offset = 0;
  for (;;) {
    // A POSIX read/write already in progress cannot be interrupted safely, but
    // a failed sibling prevents this file from issuing another bounded chunk.
    if (stop_requested != nullptr &&
        stop_requested->load(std::memory_order_acquire))
      return copied_bytes;
    ssize_t read_size;
    const size_t requested = *remaining_bytes >= buffer.size()
                                 ? buffer.size()
                                 : static_cast<size_t>(*remaining_bytes) + 1;
    do {
      read_size = pread(source_value, buffer.data(), requested, source_offset);
    } while (read_size < 0 && errno == EINTR);
    if (read_size < 0)
      ThrowCopyError("cannot read source file", source, destination, errno);
    if (read_size == 0)
      break;
    if (static_cast<uintmax_t>(read_size) > *remaining_bytes)
      throw StagingCapacityExceeded();
    if (static_cast<uintmax_t>(read_size) > std::numeric_limits<uintmax_t>::max() - copied_bytes)
      throw std::overflow_error("copied file size overflow");
    copied_bytes += static_cast<uintmax_t>(read_size);
    source_offset += read_size;

    ssize_t written = 0;
    while (written < read_size) {
      ssize_t write_size;
      do {
        write_size = write(
            destination_fd.get(), buffer.data() + written, static_cast<size_t>(read_size - written));
      } while (write_size < 0 && errno == EINTR);
      if (write_size < 0)
        ThrowCopyError("cannot write destination file", source, destination, errno);
      if (write_size == 0)
        ThrowCopyError("cannot write destination file", source, destination, EIO);
      written += write_size;
    }
    *remaining_bytes -= static_cast<uintmax_t>(read_size);
  }

  if (fchmod(destination_fd.get(), source_stat.st_mode & 0777) != 0)
    ThrowCopyError("cannot preserve destination file mode", source, destination, errno);
  if (fsync(destination_fd.get()) != 0)
    ThrowCopyError("cannot sync destination file", source, destination, errno);
  return copied_bytes;
}

uintmax_t CopyDirectoryAt(int source_fd,
                          const Path& source,
                          int destination_fd,
                          const Path& destination,
                          uintmax_t* remaining_bytes)
{
  struct stat source_stat{};
  if (fstat(source_fd, &source_stat) != 0 || !S_ISDIR(source_stat.st_mode))
    ThrowCopyError("cannot stat source directory", source, destination, errno);

  uintmax_t copied_bytes = 0;
  auto directory = OpenDirectoryStream(source_fd, source);
  for (;;) {
    errno = 0;
    const dirent* entry = readdir(directory.get());
    if (entry == nullptr) {
      if (errno != 0)
        ThrowCopyError("cannot read source directory", source, destination,
                       errno);
      break;
    }
    const std::string name(entry->d_name);
    if (name == "." || name == "..")
      continue;
    const Path source_entry = source / name;
    const Path target = destination / name;
    struct stat status{};
    if (fstatat(source_fd, name.c_str(), &status, AT_SYMLINK_NOFOLLOW) != 0)
      ThrowCopyError("cannot stat source entry", source_entry, target, errno);
    if (S_ISLNK(status.st_mode))
      throw std::runtime_error("checkpoint contains symlink");
    if (S_ISDIR(status.st_mode)) {
      const int child = openat(source_fd, name.c_str(),
                               O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);
      if (child < 0)
        ThrowCopyError("cannot open source directory", source_entry, target,
                       errno);
      const FileDescriptor child_fd(child);
      if (mkdirat(destination_fd, name.c_str(), 0700) != 0)
        ThrowCopyError("cannot create destination directory", source_entry,
                       target, errno);
      const int destination_child = openat(
          destination_fd, name.c_str(),
          O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);
      if (destination_child < 0)
        ThrowCopyError("cannot open destination directory", source_entry,
                       target, errno);
      const FileDescriptor destination_child_fd(destination_child);
      const uintmax_t child_bytes =
          CopyDirectoryAt(child_fd.get(), source_entry,
                          destination_child_fd.get(), target,
                          remaining_bytes);
      if (child_bytes > std::numeric_limits<uintmax_t>::max() - copied_bytes)
        throw std::overflow_error("copied directory size overflow");
      copied_bytes += child_bytes;
      continue;
    }
    if (!S_ISREG(status.st_mode))
      throw std::runtime_error("checkpoint contains non-regular file");
    const uintmax_t file_bytes = CopyRegularFileAt(
        source_fd, name, source_entry, destination_fd, name, target,
        remaining_bytes);
    if (file_bytes > std::numeric_limits<uintmax_t>::max() - copied_bytes)
      throw std::overflow_error("copied directory size overflow");
    copied_bytes += file_bytes;
  }
  if (fchmod(destination_fd, source_stat.st_mode & 0777) != 0)
    ThrowCopyError("cannot preserve destination directory mode", source,
                   destination, errno);
  if (fsync(destination_fd) != 0)
    ThrowCopyError("cannot sync destination directory", source, destination,
                   errno);
  return copied_bytes;
}

constexpr size_t kMaxConcurrentReferenceLinks = 8;
constexpr size_t kMinParallelReferenceLinks = 64;
constexpr ptrdiff_t kReferenceLinkHelperCount =
    kMaxConcurrentReferenceLinks - 1;
std::counting_semaphore<kReferenceLinkHelperCount>
    reference_link_helper_permits(kReferenceLinkHelperCount);

class ReferenceLinkHelperLease {
 public:
  ReferenceLinkHelperLease() = default;

  ~ReferenceLinkHelperLease()
  {
    if (count_ != 0)
      reference_link_helper_permits.release(count_);
  }

  bool TryAcquire()
  {
    if (!reference_link_helper_permits.try_acquire())
      return false;
    ++count_;
    return true;
  }

  ReferenceLinkHelperLease(const ReferenceLinkHelperLease&) = delete;
  ReferenceLinkHelperLease& operator=(const ReferenceLinkHelperLease&) = delete;

 private:
  ptrdiff_t count_ = 0;
};

constexpr size_t kMaxConcurrentDirectCopies = 8;
constexpr uintmax_t kMinParallelCopyBytes = 1024U * 1024U;
constexpr ptrdiff_t kDirectCopyPermitCount = kMaxConcurrentDirectCopies;
std::counting_semaphore<kDirectCopyPermitCount>
    direct_copy_permits(kDirectCopyPermitCount);

class DirectCopyPermitLease {
 public:
  DirectCopyPermitLease() = default;

  ~DirectCopyPermitLease()
  {
    if (count_ != 0)
      direct_copy_permits.release(count_);
  }

  bool TryAcquire()
  {
    if (!direct_copy_permits.try_acquire())
      return false;
    ++count_;
    return true;
  }

  void Acquire()
  {
    direct_copy_permits.acquire();
    ++count_;
  }

  DirectCopyPermitLease(const DirectCopyPermitLease&) = delete;
  DirectCopyPermitLease& operator=(const DirectCopyPermitLease&) = delete;

 private:
  ptrdiff_t count_ = 0;
};

struct RegularLinkEntry {
  std::string name;
  Path source;
  Path destination;
  struct stat before{};
};

struct DirectoryLinkEntry {
  std::string name;
  Path source;
  Path destination;
  struct stat before{};
};

void LinkRegularFileAt(int source_fd,
                       int destination_fd,
                       const RegularLinkEntry& entry,
                       const std::function<int(const std::string&)>&
                           operation_failure_for_testing)
{
  if (operation_failure_for_testing) {
    const int error =
        operation_failure_for_testing("link regular restore reference");
    if (error != 0)
      throw std::system_error(error, std::generic_category(),
                              "link regular restore reference");
  }
  const int source_value = openat(source_fd, entry.name.c_str(),
                                  O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
  if (source_value < 0)
    ThrowCopyError("cannot open source file", entry.source,
                   entry.destination, errno);
  const FileDescriptor source_file(source_value);
  struct stat pinned{};
  if (fstat(source_file.get(), &pinned) != 0)
    ThrowCopyError("source file changed while referenced", entry.source,
                   entry.destination, errno);
  if (!S_ISREG(pinned.st_mode) || pinned.st_dev != entry.before.st_dev ||
      pinned.st_ino != entry.before.st_ino)
    ThrowCopyError("source file changed while referenced", entry.source,
                   entry.destination, ESTALE);
  if (linkat(source_fd, entry.name.c_str(), destination_fd,
             entry.name.c_str(), 0) != 0)
    ThrowCopyError("cannot hardlink checkpoint entry", entry.source,
                   entry.destination, errno);

  struct stat current{};
  struct stat linked{};
  int error = 0;
  if (fstatat(source_fd, entry.name.c_str(), &current,
              AT_SYMLINK_NOFOLLOW) != 0)
    error = errno;
  else if (fstatat(destination_fd, entry.name.c_str(), &linked,
                   AT_SYMLINK_NOFOLLOW) != 0)
    error = errno;
  else if (!S_ISREG(current.st_mode) || !S_ISREG(linked.st_mode) ||
           current.st_dev != entry.before.st_dev ||
           current.st_ino != entry.before.st_ino ||
           linked.st_dev != pinned.st_dev || linked.st_ino != pinned.st_ino)
    error = ESTALE;
  if (error != 0) {
    (void)unlinkat(destination_fd, entry.name.c_str(), 0);
    ThrowCopyError("source entry changed while referenced", entry.source,
                   entry.destination, error);
  }
}

void LinkRegularFilesAt(int source_fd,
                        int destination_fd,
                        const std::vector<RegularLinkEntry>& entries,
                        const std::function<int(const std::string&)>&
                            operation_failure_for_testing)
{
  if (entries.empty())
    return;

  if (entries.size() < kMinParallelReferenceLinks) {
    for (const auto& entry : entries)
      LinkRegularFileAt(source_fd, destination_fd, entry,
                        operation_failure_for_testing);
    return;
  }

  // PageBroker's admitted request handler is one worker. At most seven helper
  // permits exist process-wide, so concurrent restore requests cannot multiply
  // the helper-thread count. Small directory batches stay serial to avoid
  // thread churn on deep trees.
  ReferenceLinkHelperLease helper_lease;
  size_t worker_count = 1;
  while (worker_count < kMaxConcurrentReferenceLinks &&
         worker_count < entries.size() && helper_lease.TryAcquire())
    ++worker_count;
  std::atomic_size_t next{0};
  std::atomic_bool failed{false};
  std::vector<std::exception_ptr> errors(entries.size());
  const auto worker = [&] {
    for (;;) {
      if (failed.load(std::memory_order_acquire))
        return;
      const size_t index = next.fetch_add(1, std::memory_order_relaxed);
      if (index >= entries.size())
        return;
      try {
        LinkRegularFileAt(source_fd, destination_fd, entries[index],
                          operation_failure_for_testing);
      }
      catch (...) {
        errors[index] = std::current_exception();
        failed.store(true, std::memory_order_release);
      }
    }
  };
  {
    std::vector<std::jthread> workers;
    workers.reserve(worker_count - 1);
    for (size_t index = 1; index < worker_count; ++index)
      workers.emplace_back(worker);
    worker();
  }
  for (const auto& error : errors) {
    if (error)
      std::rethrow_exception(error);
  }
}

struct DirectCopyEntry {
  std::string name;
  Path source;
  Path destination;
  struct stat before{};
  int pinned_source_fd = -1;
};

uintmax_t CopyDirectRegularFilesAt(
    int source_fd,
    int destination_fd,
    const std::vector<DirectCopyEntry>& entries,
    const std::function<int(const std::string&)>&
        operation_failure_for_testing)
{
  if (entries.empty())
    return 0;

  uintmax_t total_bytes = 0;
  for (const auto& entry : entries) {
    const uintmax_t bytes = static_cast<uintmax_t>(entry.before.st_size);
    if (bytes > std::numeric_limits<uintmax_t>::max() - total_bytes)
      throw std::overflow_error("direct restore file size overflow");
    total_bytes += bytes;
  }

  const auto copy = [&](size_t index, bool invoke_test_hook,
                        const std::atomic_bool* stop_requested) {
    const auto& entry = entries[index];
    if (invoke_test_hook && operation_failure_for_testing) {
      const int error =
          operation_failure_for_testing("copy direct restore file");
      if (error != 0)
        throw std::system_error(error, std::generic_category(),
                                "copy direct restore file");
    }
    uintmax_t remaining = static_cast<uintmax_t>(entry.before.st_size);
    const uintmax_t copied = CopyRegularFileAt(
        source_fd, entry.name, entry.source, destination_fd, entry.name,
        entry.destination, &remaining, entry.pinned_source_fd,
        &entry.before, stop_requested);
    if (copied != static_cast<uintmax_t>(entry.before.st_size) &&
        (stop_requested == nullptr ||
         !stop_requested->load(std::memory_order_acquire)))
      ThrowCopyError("source file changed size while staging", entry.source,
                     entry.destination, ESTALE);
    return copied;
  };

  DirectCopyPermitLease helper_lease;
  if (operation_failure_for_testing) {
    const int error = operation_failure_for_testing(
        "acquire direct restore copy permit");
    if (error != 0)
      throw std::system_error(error, std::generic_category(),
                              "acquire direct restore copy permit");
  }
  // Count the request thread as one of the process-wide operations. Each
  // permit holder performs work before releasing it; concurrent requests do
  // not wait while holding a second resource, so this cannot deadlock.
  helper_lease.Acquire();
  if (entries.size() < 2 || total_bytes < kMinParallelCopyBytes) {
    uintmax_t copied_bytes = 0;
    for (size_t index = 0; index < entries.size(); ++index) {
      const uintmax_t copied = copy(index, false, nullptr);
      if (copied > std::numeric_limits<uintmax_t>::max() - copied_bytes)
        throw std::overflow_error("direct restore copied size overflow");
      copied_bytes += copied;
    }
    return copied_bytes;
  }

  // Direct staging's request threads and helpers share this process-wide cap.
  size_t worker_count = 1;
  while (worker_count < kMaxConcurrentDirectCopies &&
         worker_count < entries.size() && helper_lease.TryAcquire())
    ++worker_count;
  std::atomic_size_t next{0};
  std::atomic_bool failed{false};
  std::vector<uintmax_t> copied(entries.size(), 0);
  std::vector<std::exception_ptr> errors(entries.size());
  const auto worker = [&] {
    for (;;) {
      if (failed.load(std::memory_order_acquire))
        return;
      const size_t index = next.fetch_add(1, std::memory_order_relaxed);
      if (index >= entries.size())
        return;
      try {
        copied[index] = copy(index, true, &failed);
      }
      catch (...) {
        errors[index] = std::current_exception();
        failed.store(true, std::memory_order_release);
      }
    }
  };
  {
    std::vector<std::jthread> workers;
    workers.reserve(worker_count - 1);
    for (size_t index = 1; index < worker_count; ++index)
      workers.emplace_back(worker);
    worker();
  }
  for (const auto& error : errors) {
    if (error)
      std::rethrow_exception(error);
  }
  uintmax_t copied_bytes = 0;
  for (const uintmax_t bytes : copied) {
    if (bytes > std::numeric_limits<uintmax_t>::max() - copied_bytes)
      throw std::overflow_error("direct restore copied size overflow");
    copied_bytes += bytes;
  }
  return copied_bytes;
}

void LinkDirectoryAt(int source_fd,
                     const Path& source,
                     int destination_fd,
                     const Path& destination,
                     const std::function<int(const std::string&)>&
                         operation_failure_for_testing)
{
  struct stat source_directory_stat{};
  if (fstat(source_fd, &source_directory_stat) != 0 ||
      !S_ISDIR(source_directory_stat.st_mode))
    ThrowCopyError("cannot stat source directory", source, destination, errno);

  std::vector<RegularLinkEntry> regular_entries;
  std::vector<DirectoryLinkEntry> directory_entries;
  auto directory = OpenDirectoryStream(source_fd, source);
  for (;;) {
    errno = 0;
    const dirent* entry = readdir(directory.get());
    if (entry == nullptr) {
      if (errno != 0)
        ThrowCopyError("cannot read source directory", source, destination,
                       errno);
      break;
    }
    const std::string name(entry->d_name);
    if (name == "." || name == "..")
      continue;
    const Path source_entry = source / name;
    const Path target = destination / name;
    struct stat before{};
    if (fstatat(source_fd, name.c_str(), &before, AT_SYMLINK_NOFOLLOW) != 0)
      ThrowCopyError("cannot stat source entry", source_entry, target, errno);
    if (S_ISLNK(before.st_mode))
      throw std::runtime_error("checkpoint contains symlink");
    if (S_ISDIR(before.st_mode)) {
      directory_entries.push_back({name, source_entry, target, before});
      continue;
    }
    if (!S_ISREG(before.st_mode))
      throw std::runtime_error("checkpoint contains non-regular file");
    regular_entries.push_back({name, source_entry, target, before});
  }

  LinkRegularFilesAt(source_fd, destination_fd, regular_entries,
                     operation_failure_for_testing);
  for (const auto& entry : directory_entries) {
    const int child = openat(source_fd, entry.name.c_str(),
                             O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);
    if (child < 0)
      ThrowCopyError("cannot open source directory", entry.source,
                     entry.destination, errno);
    const FileDescriptor child_fd(child);
    struct stat opened{};
    if (fstat(child_fd.get(), &opened) != 0)
      ThrowCopyError("source directory changed while referenced", entry.source,
                     entry.destination, errno);
    if (opened.st_dev != entry.before.st_dev ||
        opened.st_ino != entry.before.st_ino)
      ThrowCopyError("source directory changed while referenced", entry.source,
                     entry.destination, ESTALE);
    if (mkdirat(destination_fd, entry.name.c_str(), 0700) != 0)
      ThrowCopyError("cannot create reference directory", entry.source,
                     entry.destination, errno);
    const int destination_child = openat(
        destination_fd, entry.name.c_str(),
        O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);
    if (destination_child < 0)
      ThrowCopyError("cannot open reference directory", entry.source,
                     entry.destination, errno);
    const FileDescriptor destination_child_fd(destination_child);
    LinkDirectoryAt(child_fd.get(), entry.source,
                    destination_child_fd.get(), entry.destination,
                    operation_failure_for_testing);
  }
  if (fchmod(destination_fd, source_directory_stat.st_mode & 0777) != 0)
    ThrowCopyError("cannot preserve reference directory mode", source,
                   destination, errno);
  if (fsync(destination_fd) != 0)
    ThrowCopyError("cannot sync reference directory", source, destination,
                   errno);
}

DirectCopyAccounting CopyDirectDirectoryAt(
    int source_fd,
    const Path& source,
    int destination_fd,
    const Path& destination,
    const Path& relative,
    const DirectRestoreProcesses& processes,
    uintmax_t* remaining_bytes,
    const std::function<int(const std::string&)>&
        operation_failure_for_testing)
{
  struct stat source_stat{};
  if (fstat(source_fd, &source_stat) != 0 || !S_ISDIR(source_stat.st_mode))
    ThrowCopyError("cannot stat source directory", source, destination, errno);

  DirectCopyAccounting result;
  std::vector<DirectCopyEntry> regular_entries;
  auto directory = OpenDirectoryStream(source_fd, source);
  for (;;) {
    errno = 0;
    const dirent* entry = readdir(directory.get());
    if (entry == nullptr) {
      if (errno != 0)
        ThrowCopyError("cannot read source directory", source, destination, errno);
      break;
    }
    const std::string name(entry->d_name);
    if (name == "." || name == "..")
      continue;
    const Path source_entry = source / name;
    const Path target = destination / name;
    const Path child_relative = relative / name;
    const auto* carrier = FindDirectCarrier(child_relative, processes);
    const auto* manifest = FindDirectManifest(child_relative, processes);
    struct stat status{};
    const int stat_status = carrier != nullptr
                                ? fstat(carrier->descriptor.get(), &status)
                            : manifest != nullptr
                                ? fstat(manifest->manifest.get(), &status)
                                : fstatat(source_fd, name.c_str(), &status,
                                          AT_SYMLINK_NOFOLLOW);
    if (stat_status != 0)
      ThrowCopyError("cannot stat source entry", source_entry, target, errno);
    if (S_ISLNK(status.st_mode))
      throw std::runtime_error("checkpoint contains symlink");
    if (S_ISDIR(status.st_mode)) {
      const auto* pinned = FindDirectProcess(child_relative, processes);
      if (pinned == nullptr && operation_failure_for_testing) {
        const int error = operation_failure_for_testing(
            "open direct restore directory " + child_relative.string());
        if (error != 0)
          throw std::system_error(error, std::generic_category(),
                                  "open direct restore directory");
      }
      const int child = pinned == nullptr
                            ? openat(source_fd, name.c_str(),
                                     O_RDONLY | O_DIRECTORY | O_CLOEXEC |
                                         O_NOFOLLOW)
                            : pinned->directory.get();
      if (child < 0)
        ThrowCopyError("cannot open source directory", source_entry, target, errno);
      FileDescriptor child_fd(-1);
      if (pinned == nullptr) {
        child_fd = FileDescriptor(child);
        struct stat opened{};
        if (fstat(child_fd.get(), &opened) != 0)
          ThrowCopyError("source directory changed while staging",
                         source_entry, target, errno);
        if (!S_ISDIR(opened.st_mode) || opened.st_dev != status.st_dev ||
            opened.st_ino != status.st_ino)
          ThrowCopyError("source directory changed while staging",
                         source_entry, target, ESTALE);
      }
      if (mkdirat(destination_fd, name.c_str(), 0700) != 0)
        ThrowCopyError("cannot create destination directory", source_entry, target, errno);
      const int destination_child = openat(
          destination_fd, name.c_str(),
          O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);
      if (destination_child < 0)
        ThrowCopyError("cannot open destination directory", source_entry, target, errno);
      const FileDescriptor destination_child_fd(destination_child);
      const auto child_result = CopyDirectDirectoryAt(
          child, source_entry, destination_child_fd.get(), target,
          child_relative, processes, remaining_bytes,
          operation_failure_for_testing);
      if (child_result.copied_bytes > std::numeric_limits<uintmax_t>::max() - result.copied_bytes ||
          child_result.retained_bytes > std::numeric_limits<uintmax_t>::max() - result.retained_bytes)
        throw std::overflow_error("copied directory size overflow");
      result.copied_bytes += child_result.copied_bytes;
      result.retained_bytes += child_result.retained_bytes;
      continue;
    }
    if (!S_ISREG(status.st_mode))
      throw std::runtime_error("checkpoint contains non-regular file");
    if (carrier != nullptr) {
      const uintmax_t bytes = carrier->size;
      if (bytes > std::numeric_limits<uintmax_t>::max() - result.retained_bytes)
        throw std::overflow_error("retained carrier size overflow");
      result.retained_bytes += bytes;
      continue;
    }
    if (status.st_size < 0)
      throw std::runtime_error("checkpoint contains invalid regular file");
    const uintmax_t bytes = static_cast<uintmax_t>(status.st_size);
    if (bytes > *remaining_bytes)
      throw StagingCapacityExceeded();
    *remaining_bytes -= bytes;
    regular_entries.push_back({name, source_entry, target, status,
                               manifest == nullptr
                                   ? -1
                                   : manifest->manifest.get()});
  }
  const uintmax_t regular_bytes = CopyDirectRegularFilesAt(
      source_fd, destination_fd, regular_entries,
      operation_failure_for_testing);
  if (regular_bytes >
      std::numeric_limits<uintmax_t>::max() - result.copied_bytes)
    throw std::overflow_error("copied directory size overflow");
  result.copied_bytes += regular_bytes;
  if (fchmod(destination_fd, source_stat.st_mode & 0777) != 0)
    ThrowCopyError("cannot preserve destination directory mode", source, destination, errno);
  if (fsync(destination_fd) != 0)
    ThrowCopyError("cannot sync destination directory", source, destination, errno);
  return result;
}

void ValidateProcessPIDs(
    const std::vector<uint32_t>& cuda_namespace_pids)
{
  std::unordered_set<uint32_t> result;
  result.reserve(cuda_namespace_pids.size());
  for (const uint32_t pid : cuda_namespace_pids) {
    if (pid == 0 || !result.emplace(pid).second)
      throw std::invalid_argument("direct restore requires unique nonzero CUDA namespace PIDs");
  }
  if (result.empty() || result.size() > 64)
    throw std::invalid_argument("direct restore requires between 1 and 64 CUDA namespace PIDs");
}

size_t CountDirectProcessDescriptors(
    int source_fd,
    const Path& source,
    const std::vector<uint32_t>& namespace_pids)
{
  const int cuda_value = openat(source_fd, "cuda-custom-storage",
                                O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);
  if (cuda_value < 0)
    ThrowCopyError("cannot open CUDA CustomStorage directory", source / "cuda-custom-storage", {}, errno);
  const FileDescriptor cuda_fd(cuda_value);
  // The source directory itself remains pinned with the returned plan.
  size_t descriptors = 1;
  for (const uint32_t namespace_pid : namespace_pids) {
    const std::string leaf = "process-nspid-" + std::to_string(namespace_pid);
    const int process_value = openat(cuda_fd.get(), leaf.c_str(),
                                     O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);
    if (process_value < 0)
      ThrowCopyError("cannot open CUDA CustomStorage process directory",
                     source / "cuda-custom-storage" / leaf, {}, errno);
    const FileDescriptor process_fd(process_value);
    const int manifest_value = openat(
        process_fd.get(), storage::kManifestName,
        O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
    if (manifest_value < 0)
      ThrowCopyError("cannot validate CUDA CustomStorage manifest",
                     source / "cuda-custom-storage" / leaf /
                         storage::kManifestName,
                     {}, errno);
    const FileDescriptor manifest_fd(manifest_value);
    std::vector<storage::ManifestExtent> extents;
    std::string manifest_error;
    if (!storage::ReadManifestFile(manifest_fd.get(), &extents,
                                   &manifest_error))
      throw std::runtime_error("invalid CUDA CustomStorage manifest: " +
                               manifest_error);
    if (extents.size() > std::numeric_limits<size_t>::max() - descriptors - 2)
      throw std::overflow_error("direct restore descriptor count overflow");
    // Each process retains its directory, manifest, and every carrier file.
    descriptors += 2 + extents.size();
  }
  return descriptors;
}

DirectRestoreProcesses PinDirectProcesses(
    int source_fd,
    const Path& source,
    const std::vector<uint32_t>& namespace_pids,
    size_t max_retained_descriptors,
    uintmax_t* retained_bytes)
{
  const int cuda_value = openat(source_fd, "cuda-custom-storage",
                                O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);
  if (cuda_value < 0)
    ThrowCopyError("cannot open CUDA CustomStorage directory", source / "cuda-custom-storage", {}, errno);
  const FileDescriptor cuda_fd(cuda_value);
  DirectRestoreProcesses result;
  result.reserve(namespace_pids.size());
  size_t retained_descriptors = 1;
  for (const uint32_t namespace_pid : namespace_pids) {
    const std::string leaf = "process-nspid-" + std::to_string(namespace_pid);
    const int process_value = openat(cuda_fd.get(), leaf.c_str(),
                                     O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);
    if (process_value < 0)
      ThrowCopyError("cannot open CUDA CustomStorage process directory",
                     source / "cuda-custom-storage" / leaf, {}, errno);
    DirectRestoreProcess process{
        .namespace_pid = namespace_pid,
        .directory_name = leaf,
        .directory = FileDescriptor(process_value)};
    const int manifest_value = openat(
        process.directory.get(), storage::kManifestName,
        O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
    if (manifest_value < 0)
      ThrowCopyError("cannot validate CUDA CustomStorage manifest",
                     source / "cuda-custom-storage" / leaf /
                         storage::kManifestName,
                     {}, errno);
    process.manifest = FileDescriptor(manifest_value);
    std::vector<storage::ManifestExtent> extents;
    std::string manifest_error;
    if (!storage::ReadManifestFile(process.manifest.get(), &extents,
                                   &manifest_error))
      throw std::runtime_error("invalid CUDA CustomStorage manifest: " +
                               manifest_error);
    if (extents.size() > std::numeric_limits<size_t>::max() - retained_descriptors - 2 ||
        retained_descriptors + 2 + extents.size() > max_retained_descriptors)
      throw std::runtime_error(
          "CUDA CustomStorage descriptor count changed after admission");
    retained_descriptors += 2 + extents.size();
    process.carriers.reserve(extents.size());
    for (const auto& extent : extents) {
      const int carrier_value = openat(
          process.directory.get(), extent.filename.c_str(),
          O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
      if (carrier_value < 0)
        ThrowCopyError("cannot pin CUDA CustomStorage carrier",
                       source / "cuda-custom-storage" / leaf /
                           extent.filename,
                       {}, errno);
      struct stat status{};
      if (fstat(carrier_value, &status) != 0 ||
          !S_ISREG(status.st_mode) || status.st_size < 0 ||
          static_cast<uintmax_t>(status.st_size) != extent.size) {
        const int error = errno == 0 ? EINVAL : errno;
        close(carrier_value);
        ThrowCopyError("invalid CUDA CustomStorage carrier",
                       source / "cuda-custom-storage" / leaf /
                           extent.filename,
                       {}, error);
      }
      if (extent.size >
          std::numeric_limits<uintmax_t>::max() - *retained_bytes) {
        close(carrier_value);
        throw std::overflow_error("retained carrier size overflow");
      }
      *retained_bytes += extent.size;
      process.carriers.push_back({
          .filename = extent.filename,
          .descriptor = FileDescriptor(carrier_value),
          .size = extent.size,
          .device = static_cast<uint64_t>(status.st_dev),
          .inode = static_cast<uint64_t>(status.st_ino)});
    }
    result.push_back(std::move(process));
  }
  return result;
}

uintmax_t CopyDirectoryToPath(int source_fd,
                              const Path& source,
                              const Path& destination,
                              uintmax_t max_bytes)
{
  std::filesystem::create_directories(destination);
  const int destination_value = open(
      destination.c_str(), O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);
  if (destination_value < 0)
    ThrowCopyError("cannot open destination directory", source, destination,
                   errno);
  const FileDescriptor destination_fd(destination_value);
  uintmax_t remaining_bytes = max_bytes;
  return CopyDirectoryAt(source_fd, source, destination_fd.get(), destination,
                         &remaining_bytes);
}

DirectCopyAccounting CopyDirectDirectoryToPath(
    int source_fd,
    const Path& source,
    const Path& destination,
    const DirectRestoreProcesses& processes,
    uintmax_t max_bytes,
    const std::function<int(const std::string&)>&
        operation_failure_for_testing)
{
  std::filesystem::create_directories(destination);
  const int destination_value = open(
      destination.c_str(), O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);
  if (destination_value < 0)
    ThrowCopyError("cannot open destination directory", source, destination, errno);
  const FileDescriptor destination_fd(destination_value);
  uintmax_t remaining_bytes = max_bytes;
  return CopyDirectDirectoryAt(source_fd, source, destination_fd.get(),
                               destination, {}, processes,
                               &remaining_bytes,
                               operation_failure_for_testing);
}

bool EntryExistsAt(int parent_fd, const std::string& leaf, const Path& display)
{
  struct stat status{};
  if (fstatat(parent_fd, leaf.c_str(), &status, AT_SYMLINK_NOFOLLOW) == 0)
    return true;
  if (errno == ENOENT)
    return false;
  throw std::filesystem::filesystem_error(
      "cannot inspect checkpoint destination", display,
      std::error_code(errno, std::generic_category()));
}

std::optional<FileDescriptor> TryLockCheckpointDestination(
    int parent_fd, const Path& relative, const Path& display)
{
  const std::string lock_leaf =
      relative.filename().string() + ".pagebroker-lock";
  const int lock_value = openat(
      parent_fd, lock_leaf.c_str(),
      O_RDWR | O_CREAT | O_CLOEXEC | O_NOFOLLOW, 0600);
  if (lock_value < 0)
    throw std::filesystem::filesystem_error(
        "cannot open checkpoint destination lock", display / lock_leaf,
        std::error_code(errno, std::generic_category()));
  FileDescriptor lock_fd(lock_value);
  struct stat status{};
  if (fstat(lock_fd.get(), &status) != 0 || !S_ISREG(status.st_mode))
    throw std::filesystem::filesystem_error(
        "invalid checkpoint destination lock", display / lock_leaf,
        std::error_code(errno == 0 ? EINVAL : errno,
                        std::generic_category()));
  if (flock(lock_fd.get(), LOCK_EX | LOCK_NB) != 0) {
    if (errno == EWOULDBLOCK)
      return std::nullopt;
    throw std::filesystem::filesystem_error(
        "cannot lock checkpoint destination", display / lock_leaf,
        std::error_code(errno, std::generic_category()));
  }
  return lock_fd;
}

void RemoveTreeAt(int parent_fd,
                  const std::string& leaf,
                  const Path& display)
{
  struct stat status{};
  if (fstatat(parent_fd, leaf.c_str(), &status, AT_SYMLINK_NOFOLLOW) != 0) {
    if (errno == ENOENT)
      return;
    throw std::filesystem::filesystem_error(
        "cannot inspect cleanup entry", display,
        std::error_code(errno, std::generic_category()));
  }
  if (!S_ISDIR(status.st_mode)) {
    if (unlinkat(parent_fd, leaf.c_str(), 0) != 0)
      throw std::filesystem::filesystem_error(
          "cannot remove cleanup entry", display,
          std::error_code(errno, std::generic_category()));
    return;
  }

  const int child_value = openat(
      parent_fd, leaf.c_str(),
      O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);
  if (child_value < 0)
    throw std::filesystem::filesystem_error(
        "cannot open cleanup directory", display,
        std::error_code(errno, std::generic_category()));
  const FileDescriptor child_fd(child_value);
  auto directory = OpenDirectoryStream(child_fd.get(), display);
  for (;;) {
    errno = 0;
    const dirent* entry = readdir(directory.get());
    if (entry == nullptr) {
      if (errno != 0)
        throw std::filesystem::filesystem_error(
            "cannot read cleanup directory", display,
            std::error_code(errno, std::generic_category()));
      break;
    }
    const std::string name(entry->d_name);
    if (name == "." || name == "..")
      continue;
    RemoveTreeAt(child_fd.get(), name, display / name);
  }
  if (unlinkat(parent_fd, leaf.c_str(), AT_REMOVEDIR) != 0)
    throw std::filesystem::filesystem_error(
        "cannot remove cleanup directory", display,
        std::error_code(errno, std::generic_category()));
}

std::vector<std::string> LeavesWithPrefixAt(int parent_fd,
                                            const std::string& prefix,
                                            const Path& display)
{
  std::vector<std::string> leaves;
  auto directory = OpenDirectoryStream(parent_fd, display);
  for (;;) {
    errno = 0;
    const dirent* entry = readdir(directory.get());
    if (entry == nullptr) {
      if (errno != 0)
        throw std::filesystem::filesystem_error(
            "cannot read checkpoint destination parent", display,
            std::error_code(errno, std::generic_category()));
      break;
    }
    const std::string name(entry->d_name);
    if (name.starts_with(prefix))
      leaves.push_back(name);
  }
  return leaves;
}

void VerifyNamedDirectoryIsOpenDescriptor(int parent_fd,
                                          const std::string& leaf,
                                          int directory_fd,
                                          const Path& display)
{
  struct stat opened{};
  struct stat named{};
  errno = 0;
  if (fstat(directory_fd, &opened) != 0 ||
      fstatat(parent_fd, leaf.c_str(), &named, AT_SYMLINK_NOFOLLOW) != 0 ||
      !S_ISDIR(named.st_mode) || opened.st_dev != named.st_dev ||
      opened.st_ino != named.st_ino) {
    const int error = errno == 0 ? ESTALE : errno;
    throw std::filesystem::filesystem_error(
        "checkpoint temporary destination was replaced", display,
        std::error_code(error, std::generic_category()));
  }
}

std::string ExceptionText(const std::exception_ptr& error)
{
  try {
    std::rethrow_exception(error);
  }
  catch (const std::exception& exception) {
    return exception.what();
  }
  catch (...) {
    return "unknown failure";
  }
}
}  // namespace

PosixCopyEngine::PosixCopyEngine(Path storage_root)
    : PosixCopyEngine(std::move(storage_root), std::string{})
{
}

PosixCopyEngine::PosixCopyEngine(Path storage_root,
                                 std::string reference_owner,
                                 std::function<int(const std::string&)>
                                     operation_failure_for_testing)
    : PosixCopyEngine(std::move(storage_root), {}, {},
                      std::move(operation_failure_for_testing))
{
  if (reference_owner.empty())
    reference_owner = "standalone-" + std::to_string(getpid());
  storage::ContentDigest digest;
  std::string digest_error;
  std::string owner_digest;
  if (!digest.Update(reference_owner.data(), reference_owner.size(),
                     &digest_error) ||
      !digest.Finalize(&owner_digest, &digest_error))
    throw std::runtime_error("cannot identify PageBroker reference owner: " +
                             digest_error);

  constexpr const char* kReferenceBase = ".pagebroker-restore";
  if (mkdirat(storage_root_fd_.get(), kReferenceBase, 0700) != 0 &&
      errno != EEXIST)
    ThrowCopyError("cannot create restore reference root", storage_root_,
                   storage_root_ / kReferenceBase, errno);
  const int base_value = openat(
      storage_root_fd_.get(), kReferenceBase,
      O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);
  if (base_value < 0)
    ThrowCopyError("cannot open restore reference root", storage_root_,
                   storage_root_ / kReferenceBase, errno);
  const FileDescriptor base_fd(base_value);

  const std::string owner = owner_digest.substr(0, 32);
  const std::string lock_leaf = owner + ".lock";
  const int lock_value = openat(base_fd.get(), lock_leaf.c_str(),
                                O_RDWR | O_CREAT | O_CLOEXEC | O_NOFOLLOW,
                                0600);
  if (lock_value < 0)
    ThrowCopyError("cannot open restore reference owner lock", {},
                   storage_root_ / kReferenceBase / lock_leaf, errno);
  reference_owner_lock_fd_ = FileDescriptor(lock_value);
  struct stat lock_stat{};
  if (fstat(reference_owner_lock_fd_.get(), &lock_stat) != 0 ||
      !S_ISREG(lock_stat.st_mode))
    ThrowCopyError("invalid restore reference owner lock", {},
                   storage_root_ / kReferenceBase / lock_leaf,
                   errno == 0 ? EINVAL : errno);
  if (flock(reference_owner_lock_fd_.get(), LOCK_EX | LOCK_NB) != 0)
    ThrowCopyError("restore reference owner is active", {},
                   storage_root_ / kReferenceBase / lock_leaf, errno);

  const Path owner_display = storage_root_ / kReferenceBase / owner;
  struct stat owner_stat{};
  if (fstatat(base_fd.get(), owner.c_str(), &owner_stat,
              AT_SYMLINK_NOFOLLOW) == 0)
    RemoveTreeAt(base_fd.get(), owner, owner_display);
  else if (errno != ENOENT)
    ThrowCopyError("cannot inspect restore reference owner", {},
                   owner_display, errno);
  if (mkdirat(base_fd.get(), owner.c_str(), 0700) != 0)
    ThrowCopyError("cannot create restore reference owner", {}, owner_display,
                   errno);
  const int owner_value = openat(base_fd.get(), owner.c_str(),
                                 O_RDONLY | O_DIRECTORY | O_CLOEXEC |
                                     O_NOFOLLOW);
  if (owner_value < 0)
    ThrowCopyError("cannot open restore reference owner", {}, owner_display,
                   errno);
  reference_root_ = owner_display;
  reference_root_fd_ = FileDescriptor(owner_value);
}

PosixCopyEngine::PosixCopyEngine(
    Path storage_root,
    std::function<void()> source_opened_for_testing,
    std::function<void()> checkpoint_partial_opened_for_testing,
    std::function<int(const std::string&)> operation_failure_for_testing)
    : storage_root_(std::filesystem::weakly_canonical(std::move(storage_root))),
      storage_root_fd_(open(storage_root_.c_str(),
                            O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW)),
      source_opened_for_testing_(std::move(source_opened_for_testing)),
      checkpoint_partial_opened_for_testing_(
          std::move(checkpoint_partial_opened_for_testing)),
      operation_failure_for_testing_(
          std::move(operation_failure_for_testing))
{
  if (storage_root_fd_.get() < 0)
    throw std::filesystem::filesystem_error(
        "cannot open storage root", storage_root_,
        std::error_code(errno, std::generic_category()));
}

void
PosixCopyEngine::MaybeFailOperation(const char* operation) const
{
  if (!operation_failure_for_testing_)
    return;
  const int error = operation_failure_for_testing_(operation);
  if (error != 0)
    throw std::system_error(error, std::generic_category(), operation);
}

void
PosixCopyEngine::RenameChecked(int parent_fd,
                               const std::string& source,
                               const std::string& destination,
                               const Path& source_display,
                               const Path& destination_display,
                               const char* operation) const
{
  MaybeFailOperation(operation);
  if (renameat(parent_fd, source.c_str(), parent_fd,
               destination.c_str()) != 0)
    ThrowCopyError(operation, source_display, destination_display, errno);
}

void
PosixCopyEngine::RemoveChecked(int parent_fd,
                               const std::string& leaf,
                               const Path& display,
                               const char* operation) const
{
  MaybeFailOperation(operation);
  RemoveTreeAt(parent_fd, leaf, display);
}

void
PosixCopyEngine::SyncParentChecked(int parent_fd,
                                   const Path& display,
                                   const char* operation) const
{
  MaybeFailOperation(operation);
  if (fsync(parent_fd) != 0)
    throw std::filesystem::filesystem_error(
        operation, display,
        std::error_code(errno, std::generic_category()));
}

void
PosixCopyEngine::RecoverCheckpointDestination(int parent_fd,
                                              const Path& relative) const
{
  const std::string published = relative.filename().string();
  const std::string marker = published + ".pagebroker-partial";
  const std::string previous = PreviousLeaf(relative);
  const Path parent_display = (storage_root_ / relative).parent_path();
  const Path published_display = parent_display / published;
  const Path previous_display = parent_display / previous;
  const Path marker_display = parent_display / marker;
  if (!EntryExistsAt(parent_fd, marker, marker_display))
    return;

  const int marker_value = openat(
      parent_fd, marker.c_str(),
      O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);
  if (marker_value < 0)
    throw std::filesystem::filesystem_error(
        "invalid checkpoint publication marker", marker_display,
        std::error_code(errno, std::generic_category()));
  const FileDescriptor marker_fd(marker_value);
  (void)marker_fd;

  const bool published_exists =
      EntryExistsAt(parent_fd, published, published_display);
  const bool previous_exists =
      EntryExistsAt(parent_fd, previous, previous_display);
  if (!published_exists && previous_exists) {
    RenameChecked(parent_fd, previous, published, previous_display,
                  published_display, "recover previous checkpoint");
    SyncParentChecked(parent_fd, parent_display,
                      "sync recovered previous checkpoint");
  }
  else if (published_exists && previous_exists) {
    RemoveChecked(parent_fd, previous, previous_display,
                  "cleanup stale previous checkpoint");
    SyncParentChecked(parent_fd, parent_display,
                      "sync stale previous checkpoint cleanup");
  }

  for (const auto& leaf :
       LeavesWithPrefixAt(parent_fd, PartialPrefix(relative), parent_display)) {
    RemoveChecked(parent_fd, leaf, parent_display / leaf,
                  "cleanup stale checkpoint temporary");
  }
  SyncParentChecked(parent_fd, parent_display,
                    "sync stale checkpoint temporary cleanup");
  RemoveChecked(parent_fd, marker, marker_display,
                "cleanup stale checkpoint marker");
  SyncParentChecked(parent_fd, parent_display,
                    "sync stale checkpoint marker cleanup");
}

TransferEngineType
PosixCopyEngine::type() const
{
  return TransferEngineType::POSIX_COPY;
}

uintmax_t
PosixCopyEngine::RestoreSize(const StorageBackend& source) const
{
  const Path relative = StorageRelativePath(source, storage_root_, "source");
  const auto source_fd = OpenStorageDirectory(
      storage_root_fd_.get(), relative, storage_root_, "source");
  return DirectorySizeAt(source_fd.get(), storage_root_ / relative);
}

uintmax_t
PosixCopyEngine::StageRestore(
    const StorageBackend& source, const Path& destination,
    uintmax_t max_bytes) const
{
  const Path relative = StorageRelativePath(source, storage_root_, "source");
  const auto source_fd = OpenStorageDirectory(
      storage_root_fd_.get(), relative, storage_root_, "source");
  if (source_opened_for_testing_)
    source_opened_for_testing_();
  return CopyDirectoryToPath(source_fd.get(), storage_root_ / relative,
                             destination, max_bytes);
}

Path
PosixCopyEngine::ReferenceRegularRestore(
    const StorageBackend& source,
    const std::string& transaction_id) const
{
  if (transaction_id.empty() || transaction_id == "." ||
      transaction_id == ".." || transaction_id.find('/') != std::string::npos)
    throw std::invalid_argument("invalid restore reference transaction ID");
  const Path relative = StorageRelativePath(source, storage_root_, "source");
  if (*relative.begin() == Path(".pagebroker-restore"))
    throw std::invalid_argument("restore reference source is reserved");
  const auto source_fd = OpenStorageDirectory(
      storage_root_fd_.get(), relative, storage_root_, "source");
  if (source_opened_for_testing_)
    source_opened_for_testing_();
  const Path destination = reference_root_ / transaction_id;
  if (mkdirat(reference_root_fd_.get(), transaction_id.c_str(), 0700) != 0)
    ThrowCopyError("cannot create restore reference", storage_root_ / relative,
                   destination, errno);
  try {
    const int destination_value = openat(
        reference_root_fd_.get(), transaction_id.c_str(),
        O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);
    if (destination_value < 0)
      ThrowCopyError("cannot open restore reference", storage_root_ / relative,
                     destination, errno);
    const FileDescriptor destination_fd(destination_value);
    LinkDirectoryAt(source_fd.get(), storage_root_ / relative,
                    destination_fd.get(), destination,
                    operation_failure_for_testing_);
    VerifyNamedDirectoryIsOpenDescriptor(reference_root_fd_.get(),
                                          transaction_id,
                                          destination_fd.get(), destination);
    if (fsync(reference_root_fd_.get()) != 0)
      ThrowCopyError("cannot sync restore reference owner", {},
                     reference_root_, errno);
  }
  catch (...) {
    try {
      RemoveTreeAt(reference_root_fd_.get(), transaction_id, destination);
    }
    catch (...) {
      // Preserve the original reference failure. The owner-scoped startup
      // recovery will remove this pre-mutation partial tree.
    }
    throw;
  }
  return destination;
}

DirectRestorePlan
PosixCopyEngine::PrepareDirectRestore(
    const StorageBackend& source,
    const std::vector<uint32_t>& cuda_namespace_pids,
    size_t max_retained_descriptors) const
{
  const Path relative = StorageRelativePath(source, storage_root_, "source");
  auto source_fd = OpenStorageDirectory(
      storage_root_fd_.get(), relative, storage_root_, "source");
  if (source_opened_for_testing_)
    source_opened_for_testing_();
  ValidateProcessPIDs(cuda_namespace_pids);
  uintmax_t retained_bytes = 0;
  auto processes = PinDirectProcesses(
      source_fd.get(), storage_root_ / relative, cuda_namespace_pids,
      max_retained_descriptors, &retained_bytes);
  const auto accounting = DirectDirectorySizeAt(
      source_fd.get(), storage_root_ / relative, {}, processes);
  if (accounting.retained_bytes != retained_bytes)
    throw std::runtime_error("direct restore carrier accounting mismatch");
  return {.source_directory = std::move(source_fd),
          .processes = std::move(processes),
          .source_display = storage_root_ / relative,
          .cuda_namespace_pids = cuda_namespace_pids,
          .copied_bytes = accounting.copied_bytes,
          .retained_bytes = accounting.retained_bytes};
}

size_t
PosixCopyEngine::DirectRestoreDescriptorCount(
    const StorageBackend& source,
    const std::vector<uint32_t>& cuda_namespace_pids) const
{
  const Path relative = StorageRelativePath(source, storage_root_, "source");
  const auto source_fd = OpenStorageDirectory(
      storage_root_fd_.get(), relative, storage_root_, "source");
  ValidateProcessPIDs(cuda_namespace_pids);
  return CountDirectProcessDescriptors(
      source_fd.get(), storage_root_ / relative, cuda_namespace_pids);
}

DirectRestoreStage
PosixCopyEngine::StageDirectRestore(
    DirectRestorePlan plan,
    const Path& destination) const
{
  const auto copied = CopyDirectDirectoryToPath(
      plan.source_directory.get(), plan.source_display, destination,
      plan.processes, plan.copied_bytes,
      operation_failure_for_testing_);
  return {.source_directory = std::move(plan.source_directory),
          .processes = std::move(plan.processes),
          .copied_bytes = copied.copied_bytes,
          .retained_bytes = copied.retained_bytes};
}

void
PosixCopyEngine::ValidateCheckpointDestination(const StorageBackend& destination) const
{
  const Path relative =
      StorageRelativePath(destination, storage_root_, "destination");
  try {
    (void)OpenStorageParent(storage_root_fd_.get(), relative, storage_root_,
                            false);
  } catch (const std::filesystem::filesystem_error& error) {
    if (error.code().value() == ELOOP || error.code().value() == ENOTDIR)
      throw std::invalid_argument("destination contains symlink");
    throw;
  }
}

bool
PosixCopyEngine::CheckpointDestinationConflicts(const StorageBackend& destination) const
{
  std::lock_guard lock(checkpoint_publication_mutex_);
  const Path relative =
      StorageRelativePath(destination, storage_root_, "destination");
  const auto parent = OpenStorageParent(storage_root_fd_.get(), relative,
                                        storage_root_, false);
  if (parent.get() < 0)
    return false;
  const auto destination_lock = TryLockCheckpointDestination(
      parent.get(), relative, (storage_root_ / relative).parent_path());
  if (!destination_lock)
    return true;
  struct stat status{};
  const std::string partial =
      relative.filename().string() + ".pagebroker-partial";
  if (fstatat(parent.get(), partial.c_str(), &status,
              AT_SYMLINK_NOFOLLOW) == 0) {
    if (!S_ISDIR(status.st_mode))
      return true;
    RecoverCheckpointDestination(parent.get(), relative);
    return false;
  }
  if (errno == ENOENT)
    return false;
  throw std::filesystem::filesystem_error(
      "cannot inspect checkpoint destination", storage_root_ / relative,
      std::error_code(errno, std::generic_category()));
}

void
PosixCopyEngine::PublishCheckpoint(const Path& source, const StorageBackend& destination) const
{
  std::lock_guard lock(checkpoint_publication_mutex_);
  const Path relative =
      StorageRelativePath(destination, storage_root_, "destination");
  const auto parent = OpenStorageParent(storage_root_fd_.get(), relative,
                                        storage_root_, true);
  const auto destination_lock = TryLockCheckpointDestination(
      parent.get(), relative, (storage_root_ / relative).parent_path());
  if (!destination_lock)
    throw std::filesystem::filesystem_error(
        "checkpoint destination is being published", storage_root_ / relative,
        std::error_code(EBUSY, std::generic_category()));
  RecoverCheckpointDestination(parent.get(), relative);
  const std::string published = relative.filename().string();
  const std::string publication_lock =
      published + ".pagebroker-partial";
  const std::string partial = RandomPartialLeaf(relative);
  const std::string previous = PreviousLeaf(relative);
  const Path published_display = storage_root_ / relative;
  const Path publication_lock_display =
      published_display.parent_path() / publication_lock;
  const Path partial_display = published_display.parent_path() / partial;
  bool owns_publication_lock = false;
  bool owns_partial_name = false;
  bool previous_retained = false;
  bool new_checkpoint_published = false;
  try {
    const int source_value = open(
        source.c_str(), O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);
    if (source_value < 0)
      ThrowCopyError("cannot open source directory", source, partial_display,
                     errno);
    const FileDescriptor source_fd(source_value);

    if (mkdirat(parent.get(), publication_lock.c_str(), 0700) != 0)
      ThrowCopyError("checkpoint destination conflicts", source,
                     publication_lock_display, errno);
    owns_publication_lock = true;
    SyncParentChecked(parent.get(), published_display.parent_path(),
                      "sync checkpoint publication marker");
    if (mkdirat(parent.get(), partial.c_str(), 0700) != 0)
      ThrowCopyError("cannot create checkpoint temporary destination", source,
                     partial_display, errno);
    owns_partial_name = true;
    const int partial_value = openat(
        parent.get(), partial.c_str(),
        O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);
    if (partial_value < 0)
      ThrowCopyError("cannot open checkpoint temporary destination", source,
                     partial_display, errno);
    const FileDescriptor partial_fd(partial_value);
    if (checkpoint_partial_opened_for_testing_)
      checkpoint_partial_opened_for_testing_();
    uintmax_t remaining = std::numeric_limits<uintmax_t>::max();
    (void)CopyDirectoryAt(source_fd.get(), source, partial_fd.get(),
                          partial_display, &remaining);
    VerifyNamedDirectoryIsOpenDescriptor(
        parent.get(), partial, partial_fd.get(), partial_display);

    if (EntryExistsAt(parent.get(), published, published_display)) {
      RenameChecked(
          parent.get(), published, previous, published_display,
          published_display.parent_path() / previous,
          "retain previous checkpoint");
      previous_retained = true;
      SyncParentChecked(parent.get(), published_display.parent_path(),
                        "sync retained previous checkpoint");
    }

    try {
      RenameChecked(parent.get(), partial, published, partial_display,
                    published_display, "publish checkpoint");
    }
    catch (...) {
      const auto publish_error = std::current_exception();
      if (previous_retained) {
        try {
          RenameChecked(parent.get(), previous, published,
                        published_display.parent_path() / previous,
                        published_display, "rollback previous checkpoint");
          previous_retained = false;
          SyncParentChecked(parent.get(), published_display.parent_path(),
                            "sync rolled back previous checkpoint");
        }
        catch (...) {
          previous_retained = true;
          throw std::runtime_error(
              "checkpoint publication failed: " + ExceptionText(publish_error) +
              "; previous checkpoint rollback failed: " +
              ExceptionText(std::current_exception()) +
              "; recovery state retained");
        }
      }
      std::rethrow_exception(publish_error);
    }
    owns_partial_name = false;
    new_checkpoint_published = true;
    SyncParentChecked(parent.get(), published_display.parent_path(),
                      "sync published checkpoint");

    if (previous_retained) {
      RemoveChecked(parent.get(), previous,
                    published_display.parent_path() / previous,
                    "cleanup previous checkpoint");
      previous_retained = false;
      SyncParentChecked(parent.get(), published_display.parent_path(),
                        "sync previous checkpoint cleanup");
    }
    RemoveChecked(parent.get(), publication_lock, publication_lock_display,
                  "cleanup checkpoint publication marker");
    owns_publication_lock = false;
    SyncParentChecked(parent.get(), published_display.parent_path(),
                      "sync checkpoint publication marker cleanup");
  }
  catch (...) {
    const auto primary_error = std::current_exception();
    if (new_checkpoint_published || previous_retained)
      std::rethrow_exception(primary_error);
    try {
      if (owns_partial_name)
        RemoveChecked(parent.get(), partial, partial_display,
                      "cleanup failed checkpoint temporary");
      if (owns_publication_lock)
        RemoveChecked(parent.get(), publication_lock,
                      publication_lock_display,
                      "cleanup failed checkpoint marker");
      SyncParentChecked(parent.get(), published_display.parent_path(),
                        "sync failed checkpoint cleanup");
    }
    catch (...) {
      throw std::runtime_error(
          "checkpoint publication failed: " + ExceptionText(primary_error) +
          "; cleanup failed: " + ExceptionText(std::current_exception()) +
          "; recovery state retained");
    }
    std::rethrow_exception(primary_error);
  }
}

uintmax_t
PosixCopyEngine::CopyDirectory(const Path& source, const Path& destination) const
{
  const int source_value =
      open(source.c_str(), O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);
  if (source_value < 0)
    ThrowCopyError("cannot open source directory", source, destination, errno);
  const FileDescriptor source_fd(source_value);
  return CopyDirectoryToPath(source_fd.get(), source, destination,
                             std::numeric_limits<uintmax_t>::max());
}
}  // namespace snapshot::pagebroker
